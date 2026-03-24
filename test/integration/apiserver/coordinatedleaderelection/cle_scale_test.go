/*
Copyright 2026 The Kubernetes Authors.

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

package leaderelection

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	v1beta1 "k8s.io/api/coordination/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	genericfeatures "k8s.io/apiserver/pkg/features"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	kubernetes "k8s.io/client-go/kubernetes"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	apiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/test/integration/framework"
	"k8s.io/utils/ptr"
)

// TestCLEvsPlainLeaseElection compares election latency between CLE-coordinated
// election and plain lease-based election, with and without node lease churn.
// This is the primary benchmark for measuring CLE overhead.
func TestCLEvsPlainLeaseElection(t *testing.T) {
	nodeLeasesCounts := []int{0, 500}

	for _, numNodeLeases := range nodeLeasesCounts {
		t.Run(fmt.Sprintf("node_leases_%d", numNodeLeases), func(t *testing.T) {
			// --- Plain lease election (CLE off) ---
			t.Run("plain_lease", func(t *testing.T) {
				featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, genericfeatures.CoordinatedLeaderElection, false)

				server, err := apiservertesting.StartTestServer(t, apiservertesting.NewDefaultTestServerOptions(), nil, framework.SharedEtcd())
				if err != nil {
					t.Fatal(err)
				}
				defer server.TearDownFn()

				clientset := kubernetes.NewForConfigOrDie(server.ClientConfig)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				if numNodeLeases > 0 {
					createNodeLeases(ctx, t, clientset, numNodeLeases)
					churnCtx, churnCancel := context.WithCancel(ctx)
					defer churnCancel()
					go simulateNodeLeaseRenewals(churnCtx, t, clientset, numNodeLeases)
				}

				cletest := setupCLE(server.ClientConfig, ctx, t)
				defer cletest.cleanup()

				// Plain election: first controller to acquire the lease wins.
				electionStart := time.Now()
				go cletest.createAndRunFakeLegacyController("plain-1", "default", "bench-lease")
				cletest.pollForLease(ctx, "bench-lease", "default", ptr.To("plain-1"))
				t.Logf("Plain initial election: %v (node leases: %d)", time.Since(electionStart), numNodeLeases)

				// Kill leader, force-expire, measure re-election.
				cletest.cancelController("plain-1", "default")
				forceExpireLease(ctx, t, clientset, "default", "bench-lease")

				go cletest.createAndRunFakeLegacyController("plain-2", "default", "bench-lease")
				reelectionStart := time.Now()
				cletest.pollForLease(ctx, "bench-lease", "default", ptr.To("plain-2"))
				t.Logf("Plain re-election: %v", time.Since(reelectionStart))
			})

			// --- CLE election (CLE on) ---
			t.Run("coordinated", func(t *testing.T) {
				featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, genericfeatures.CoordinatedLeaderElection, true)

				flags := []string{fmt.Sprintf("--runtime-config=%s=true", v1beta1.SchemeGroupVersion)}
				server, err := apiservertesting.StartTestServer(t, apiservertesting.NewDefaultTestServerOptions(), flags, framework.SharedEtcd())
				if err != nil {
					t.Fatal(err)
				}
				defer server.TearDownFn()

				clientset := kubernetes.NewForConfigOrDie(server.ClientConfig)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				if numNodeLeases > 0 {
					createNodeLeases(ctx, t, clientset, numNodeLeases)
					churnCtx, churnCancel := context.WithCancel(ctx)
					defer churnCancel()
					go simulateNodeLeaseRenewals(churnCtx, t, clientset, numNodeLeases)
				}

				cletest := setupCLE(server.ClientConfig, ctx, t)
				defer cletest.cleanup()

				electionStart := time.Now()
				go cletest.createAndRunFakeController("cle-1", "default", "bench-lease", "1.20.0", "1.20.0", v1.OldestEmulationVersion)
				cletest.pollForLease(ctx, "bench-lease", "default", ptr.To("cle-1"))
				t.Logf("CLE initial election: %v (node leases: %d)", time.Since(electionStart), numNodeLeases)

				// Kill leader, force-expire, measure re-election with a second candidate.
				go cletest.createAndRunFakeController("cle-2", "default", "bench-lease", "1.20.0", "1.20.0", v1.OldestEmulationVersion)
				// Wait for cle-2 to register as a candidate before killing the leader.
				pollForLeaseCandidate(ctx, t, clientset, "default", "cle-2")

				cletest.cancelController("cle-1", "default")
				forceExpireLease(ctx, t, clientset, "default", "bench-lease")

				reelectionStart := time.Now()
				cletest.pollForLease(ctx, "bench-lease", "default", ptr.To("cle-2"))
				t.Logf("CLE re-election: %v", time.Since(reelectionStart))
			})
		})
	}
}

// TestCLEWithNodeLeaseChurn verifies that the CLE controller handles high-volume
// node lease updates without impacting election latency. This simulates the
// kube-node-lease namespace activity in a large cluster.
func TestCLEWithNodeLeaseChurn(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, genericfeatures.CoordinatedLeaderElection, true)

	flags := []string{fmt.Sprintf("--runtime-config=%s=true", v1beta1.SchemeGroupVersion)}
	server, err := apiservertesting.StartTestServer(t, apiservertesting.NewDefaultTestServerOptions(), flags, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	clientset := kubernetes.NewForConfigOrDie(server.ClientConfig)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const numNodeLeases = 500
	createNodeLeases(ctx, t, clientset, numNodeLeases)

	// Start continuous node lease renewal in the background to generate churn.
	churnCtx, churnCancel := context.WithCancel(ctx)
	defer churnCancel()
	go simulateNodeLeaseRenewals(churnCtx, t, clientset, numNodeLeases)

	// Now run an actual CLE election and measure how long it takes.
	cletest := setupCLE(server.ClientConfig, ctx, t)
	defer cletest.cleanup()

	electionStart := time.Now()
	go cletest.createAndRunFakeController("candidate-1", "default", "scale-test-lease", "1.20.0", "1.20.0", v1.OldestEmulationVersion)
	cletest.pollForLease(ctx, "scale-test-lease", "default", ptr.To("candidate-1"))
	initialElection := time.Since(electionStart)
	t.Logf("Initial election took %v (with %d node leases churning)", initialElection, numNodeLeases)

	// Add a better candidate and measure preemption latency.
	preemptStart := time.Now()
	go cletest.createAndRunFakeController("candidate-2", "default", "scale-test-lease", "1.19.0", "1.19.0", v1.OldestEmulationVersion)
	cletest.pollForLease(ctx, "scale-test-lease", "default", ptr.To("candidate-2"))
	preemptLatency := time.Since(preemptStart)
	t.Logf("Preemption to better candidate took %v", preemptLatency)

	// Kill the leader and measure re-election latency.
	cletest.cancelController("candidate-2", "default")
	forceExpireLease(ctx, t, clientset, "default", "scale-test-lease")

	reelectionStart := time.Now()
	cletest.pollForLease(ctx, "scale-test-lease", "default", ptr.To("candidate-1"))
	reelectionLatency := time.Since(reelectionStart)
	t.Logf("Re-election after leader loss took %v", reelectionLatency)

	// Sanity check: election latencies should be reasonable even under churn.
	// These are generous bounds — the point is to catch regressions, not assert tight timings.
	const maxElectionLatency = 20 * time.Second
	if initialElection > maxElectionLatency {
		t.Errorf("Initial election too slow: %v > %v", initialElection, maxElectionLatency)
	}
	if preemptLatency > maxElectionLatency {
		t.Errorf("Preemption too slow: %v > %v", preemptLatency, maxElectionLatency)
	}
	if reelectionLatency > maxElectionLatency {
		t.Errorf("Re-election too slow: %v > %v", reelectionLatency, maxElectionLatency)
	}
}

// TestCLEElectionLatencyScaling measures how election latency scales with the number
// of candidates contending for the same lease.
func TestCLEElectionLatencyScaling(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, genericfeatures.CoordinatedLeaderElection, true)

	flags := []string{fmt.Sprintf("--runtime-config=%s=true", v1beta1.SchemeGroupVersion)}
	server, err := apiservertesting.StartTestServer(t, apiservertesting.NewDefaultTestServerOptions(), flags, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	candidateCounts := []int{3, 10, 25}
	for _, n := range candidateCounts {
		t.Run(fmt.Sprintf("%d_candidates", n), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			cletest := setupCLE(server.ClientConfig, ctx, t)
			defer cletest.cleanup()

			leaseName := fmt.Sprintf("scale-%d", n)
			// The candidate with the lowest emulation version should win.
			// candidate-0 gets 1.10.0, everyone else gets 1.20.0.
			electionStart := time.Now()
			for i := 0; i < n; i++ {
				emulationVersion := "1.20.0"
				if i == 0 {
					emulationVersion = "1.10.0"
				}
				name := fmt.Sprintf("candidate-%d-%d", n, i)
				go cletest.createAndRunFakeController(name, "default", leaseName, "1.20.0", emulationVersion, v1.OldestEmulationVersion)
			}

			expectedWinner := fmt.Sprintf("candidate-%d-0", n)
			cletest.pollForLease(ctx, leaseName, "default", ptr.To(expectedWinner))
			elapsed := time.Since(electionStart)
			t.Logf("Election with %d candidates took %v, winner: %s", n, elapsed, expectedWinner)
		})
	}
}

// TestCLEMultipleConcurrentLeases verifies that the CLE controller can manage
// elections for multiple leases simultaneously.
func TestCLEMultipleConcurrentLeases(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, genericfeatures.CoordinatedLeaderElection, true)

	flags := []string{fmt.Sprintf("--runtime-config=%s=true", v1beta1.SchemeGroupVersion)}
	server, err := apiservertesting.StartTestServer(t, apiservertesting.NewDefaultTestServerOptions(), flags, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cletest := setupCLE(server.ClientConfig, ctx, t)
	defer cletest.cleanup()

	// Simulate multiple controller types each doing CLE (like KCM + scheduler + custom controllers).
	const numLeases = 10
	const candidatesPerLease = 3

	electionStart := time.Now()
	for i := 0; i < numLeases; i++ {
		leaseName := fmt.Sprintf("multi-lease-%d", i)
		for j := 0; j < candidatesPerLease; j++ {
			name := fmt.Sprintf("ctrl-%d-%d", i, j)
			emulationVersion := "1.20.0"
			if j == 0 {
				emulationVersion = "1.10.0" // j=0 should win each lease
			}
			go cletest.createAndRunFakeController(name, "default", leaseName, "1.20.0", emulationVersion, v1.OldestEmulationVersion)
		}
	}

	// Wait for all leases to elect their expected winner.
	for i := 0; i < numLeases; i++ {
		leaseName := fmt.Sprintf("multi-lease-%d", i)
		expectedWinner := fmt.Sprintf("ctrl-%d-0", i)
		cletest.pollForLease(ctx, leaseName, "default", ptr.To(expectedWinner))
	}
	elapsed := time.Since(electionStart)
	t.Logf("All %d leases elected in %v (%d candidates each)", numLeases, elapsed, candidatesPerLease)
}

// createNodeLeases creates simulated node heartbeat leases in kube-node-lease.
func createNodeLeases(ctx context.Context, t *testing.T, clientset kubernetes.Interface, count int) {
	t.Helper()
	_, err := clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: corev1.NamespaceNodeLease},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Logf("namespace create (may already exist): %v", err)
	}

	t.Logf("Creating %d simulated node leases", count)
	for i := 0; i < count; i++ {
		lease := &v1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("node-%04d", i),
				Namespace: corev1.NamespaceNodeLease,
			},
			Spec: v1.LeaseSpec{
				HolderIdentity:       ptr.To(fmt.Sprintf("node-%04d", i)),
				LeaseDurationSeconds: ptr.To[int32](40),
				RenewTime:            &metav1.MicroTime{Time: time.Now()},
			},
		}
		_, err := clientset.CoordinationV1().Leases(corev1.NamespaceNodeLease).Create(ctx, lease, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create node lease %d: %v", i, err)
		}
	}
}

// simulateNodeLeaseRenewals continuously updates node leases to simulate kubelet heartbeats.
func simulateNodeLeaseRenewals(ctx context.Context, t *testing.T, clientset kubernetes.Interface, numLeases int) {
	t.Helper()
	// Cycle through leases, renewing each one. In a real cluster each kubelet
	// renews its own lease every ~10s. Here we just create continuous churn.
	i := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		name := fmt.Sprintf("node-%04d", i%numLeases)
		lease, err := clientset.CoordinationV1().Leases(corev1.NamespaceNodeLease).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			continue
		}
		lease.Spec.RenewTime = &metav1.MicroTime{Time: time.Now()}
		_, err = clientset.CoordinationV1().Leases(corev1.NamespaceNodeLease).Update(ctx, lease, metav1.UpdateOptions{})
		if err != nil {
			// Conflicts are expected under churn, just keep going.
			continue
		}
		i++

		// Pace the renewals to avoid overwhelming the test apiserver.
		err = wait.PollUntilContextTimeout(ctx, 2*time.Millisecond, time.Second, true, func(ctx context.Context) (bool, error) {
			return true, nil
		})
		if err != nil {
			return
		}
	}
}

// forceExpireLease sets the renewTime far in the past so CLE treats the lease as expired.
func forceExpireLease(ctx context.Context, t *testing.T, clientset kubernetes.Interface, namespace, name string) {
	t.Helper()
	lease, err := clientset.CoordinationV1().Leases(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	lease.Spec.RenewTime = &metav1.MicroTime{Time: time.Now().Add(-30 * time.Second)}
	_, err = clientset.CoordinationV1().Leases(namespace).Update(ctx, lease, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
}

// pollForLeaseCandidate waits until a LeaseCandidate with the given name exists.
func pollForLeaseCandidate(ctx context.Context, t *testing.T, clientset kubernetes.Interface, namespace, name string) {
	t.Helper()
	err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		_, err := clientset.CoordinationV1beta1().LeaseCandidates(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("timeout waiting for LeaseCandidate %s/%s", namespace, name)
	}
}
