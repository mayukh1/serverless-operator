//go:build upgrade
// +build upgrade

/*
Copyright 2020 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kitchensink

import (
	"log"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/openshift-knative/serverless-operator/test"
	"github.com/openshift-knative/serverless-operator/test/kitchensinke2e/features"
	"github.com/openshift-knative/serverless-operator/test/upgrade"
	"github.com/openshift-knative/serverless-operator/test/upgrade/installation"
	_ "knative.dev/pkg/system/testing"
	pkgupgrade "knative.dev/pkg/test/upgrade"
	"knative.dev/reconciler-test/pkg/environment"
	"knative.dev/reconciler-test/pkg/feature"

	// Make sure to initialize flags from knative.dev/pkg/test before parsing them.
	pkgTest "knative.dev/pkg/test"
)

var global environment.GlobalEnvironment

func TestMain(m *testing.M) {
	restConfig, err := pkgTest.Flags.ClientConfig.GetRESTConfig()
	if err != nil {
		log.Fatal("Error building client config: ", err)
	}

	// Getting the rest config explicitly and passing it further will prevent re-initializing the flagset
	// in NewStandardGlobalEnvironment().
	global = environment.NewStandardGlobalEnvironment(func(cfg environment.Configuration) environment.Configuration {
		cfg.Config = restConfig
		return cfg
	})

	// Run the tests.
	os.Exit(m.Run())
}

// TestKitchensink tests as many Knative resources as possible during upgrades.
// It does a series of upgrades according to CSVs passed via test flags. For each
// upgrade it takes a random subset of features from the whole group, installs them
// and verifies their readiness. The size of each subset is N / num_of_upgrades where
// N is the overall size of the feature set. The last subset includes any remaining
// features that didn't fit into previous groups.
// Readiness of all features is checked after last upgrade. Additional checks at this
// point include modifying all resources and deleting the test namespaces.
func TestKitchensink(t *testing.T) {
	ctx := test.SetupClusterAdmin(t)
	test.CleanupOnInterrupt(t, func() { test.CleanupAll(t, ctx) })

	// Add feature sets to be tested during upgrades.
	featureSets := []feature.FeatureSet{
		features.BrokerFeatureSetWithBrokerDLS(true),
		features.BrokerFeatureSetWithTriggerDLS(true),
		features.ChannelFeatureSet(true),
		features.SequenceNoReplyFeatureSet(true),
		features.SequenceGlobalReplyFeatureSet(true),
		features.ParallelNoReplyFeatureSet(true),
		features.ParallelGlobalReplyFeatureSet(true),
	}

	var featureGroup FeatureWithEnvironmentGroup
	for _, fs := range featureSets {
		for _, f := range fs.Features {
			featureGroup = append(featureGroup, &FeatureWithEnvironment{Feature: f, Global: global})
		}
	}

	// Shuffle the features so that different features are installed at each stage
	// every time we run the tests. This is to cover more combinations of Features
	// and Serverless versions while keeping the payload small enough for the cluster.
	rand.Seed(time.Now().UnixNano())
	rand.Shuffle(len(featureGroup), func(i, j int) { featureGroup[i], featureGroup[j] = featureGroup[j], featureGroup[i] })

	sources := strings.Split(strings.Trim(test.Flags.CatalogSource, ","), ",")
	csvs := strings.Split(strings.Trim(test.Flags.CSV, ","), ",")
	if len(sources) != len(csvs) {
		t.Fatal("The number of operator sources and CSVs for upgrades must match")
	}

	// Split features across upgrades.
	groups := featureGroup.Split(len(csvs))

	for i, csv := range csvs {
		_, toVersion, _ := strings.Cut(csv, ".")

		t.Run("UpgradeTo "+toVersion, func(t *testing.T) {
			// Run these tests after each upgrade.
			post := []pkgupgrade.Operation{
				upgrade.VerifyPostInstallJobs(ctx, upgrade.VerifyPostJobsConfig{
					Namespace: "knative-serving",
				}),
				upgrade.VerifyPostInstallJobs(ctx, upgrade.VerifyPostJobsConfig{
					Namespace: "knative-eventing",
				}),
			}
			// In the last step. Run also post-upgrade tests for all features.
			if i == len(csvs)-1 {
				post = append(post, ModifyResourcesTest(ctx))
				post = append(post, featureGroup.PostUpgradeTests()...)
			}

			source := sources[i]

			suite := pkgupgrade.Suite{
				Tests: pkgupgrade.Tests{
					// Run pre-upgrade tests only for given sub-group
					PreUpgrade:  groups[i].PreUpgradeTests(),
					PostUpgrade: post,
				},
				Installations: pkgupgrade.Installations{
					UpgradeWith: []pkgupgrade.Operation{
						pkgupgrade.NewOperation("UpgradeServerless", func(c pkgupgrade.Context) {
							if err := installation.UpgradeServerlessTo(ctx, csv, source); err != nil {
								c.T.Error("Serverless upgrade failed:", err)
							}
						}),
					},
				},
			}
			suite.Execute(pkgupgrade.Configuration{T: t})
		})
	}
}

func ModifyResourcesTest(ctx *test.Context) pkgupgrade.Operation {
	return pkgupgrade.NewOperation("ModifyResourcesTest", func(c pkgupgrade.Context) {
		// Intentionally don't use t.Parallel() to make the test run before parallel tests.
		// The parallel tests delete namespaces so patching the resources must be done earlier.
		if err := PatchKnativeResources(ctx); err != nil {
			c.T.Error(err)
		}
	})
}
