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
	"strings"
	"sync/atomic"
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

// --- Happy path tests ---

// TestCLERenewalOverhead measures steady-state renewal API call overhead.
// With the fast path from PR #138064, CLE renewal should use a single Update
// per cycle (no Get), matching plain lease LE.
func TestCLERenewalOverhead(t *testing.T) {
	const renewalDuration = 15 * time.Second

	// --- Plain lease renewal ---
	t.Run("plain", func(t *testing.T) {
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, genericfeatures.CoordinatedLeaderElection, false)

		server, err := apiservertesting.StartTestServer(t, apiservertesting.NewDefaultTestServerOptions(), nil, framework.SharedEtcd())
		if err != nil {
			t.Fatal(err)
		}
		defer server.TearDownFn()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cletest := setupCLE(server.ClientConfig, ctx, t)
		defer cletest.cleanup()

		go cletest.createAndRunFakeLegacyController("plain-renew-1", "default", "renew-bench-plain")
		cletest.pollForLease(ctx, "renew-bench-plain", "default", ptr.To("plain-renew-1"))

		// Count renewals over the measurement window.
		renewalCount := countLeaseRenewals(ctx, t, cletest.clientset, "default", "renew-bench-plain", renewalDuration)
		t.Logf("Plain: %d renewals in %v (%.1f/s)", renewalCount, renewalDuration, float64(renewalCount)/renewalDuration.Seconds())
	})

	// --- CLE renewal ---
	t.Run("coordinated", func(t *testing.T) {
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

		go cletest.createAndRunFakeController("cle-renew-1", "default", "renew-bench-cle", "1.20.0", "1.20.0", v1.OldestEmulationVersion)
		cletest.pollForLease(ctx, "renew-bench-cle", "default", ptr.To("cle-renew-1"))

		renewalCount := countLeaseRenewals(ctx, t, cletest.clientset, "default", "renew-bench-cle", renewalDuration)
		t.Logf("CLE: %d renewals in %v (%.1f/s)", renewalCount, renewalDuration, float64(renewalCount)/renewalDuration.Seconds())
	})
}

// countLeaseRenewals polls the lease and counts how many times RenewTime changes.
func countLeaseRenewals(ctx context.Context, t *testing.T, clientset kubernetes.Interface, namespace, name string, duration time.Duration) int {
	t.Helper()
	var count int
	var lastRenew time.Time

	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		lease, err := clientset.CoordinationV1().Leases(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if lease.Spec.RenewTime != nil && !lease.Spec.RenewTime.Time.Equal(lastRenew) {
			count++
			lastRenew = lease.Spec.RenewTime.Time
		}
		time.Sleep(200 * time.Millisecond)
	}
	return count
}

// TestCLEAcquisitionAtScale measures time to elect leaders for many leases concurrently.
func TestCLEAcquisitionAtScale(t *testing.T) {
	const numLeases = 20
	const candidatesPerLease = 2

	// --- Plain lease acquisition ---
	t.Run("plain", func(t *testing.T) {
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, genericfeatures.CoordinatedLeaderElection, false)

		server, err := apiservertesting.StartTestServer(t, apiservertesting.NewDefaultTestServerOptions(), nil, framework.SharedEtcd())
		if err != nil {
			t.Fatal(err)
		}
		defer server.TearDownFn()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cletest := setupCLE(server.ClientConfig, ctx, t)
		defer cletest.cleanup()

		start := time.Now()
		for i := 0; i < numLeases; i++ {
			leaseName := fmt.Sprintf("acq-plain-%d", i)
			name := fmt.Sprintf("plain-ctrl-%d", i)
			go cletest.createAndRunFakeLegacyController(name, "default", leaseName)
		}

		for i := 0; i < numLeases; i++ {
			leaseName := fmt.Sprintf("acq-plain-%d", i)
			name := fmt.Sprintf("plain-ctrl-%d", i)
			cletest.pollForLease(ctx, leaseName, "default", ptr.To(name))
		}
		elapsed := time.Since(start)
		t.Logf("Plain: %d leases acquired in %v", numLeases, elapsed)
	})

	// --- CLE acquisition ---
	t.Run("coordinated", func(t *testing.T) {
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

		start := time.Now()
		for i := 0; i < numLeases; i++ {
			leaseName := fmt.Sprintf("acq-cle-%d", i)
			for j := 0; j < candidatesPerLease; j++ {
				name := fmt.Sprintf("cle-ctrl-%d-%d", i, j)
				emulationVersion := "1.20.0"
				if j == 0 {
					emulationVersion = "1.19.0" // j=0 should win
				}
				go cletest.createAndRunFakeController(name, "default", leaseName, "1.20.0", emulationVersion, v1.OldestEmulationVersion)
			}
		}

		for i := 0; i < numLeases; i++ {
			leaseName := fmt.Sprintf("acq-cle-%d", i)
			expectedWinner := fmt.Sprintf("cle-ctrl-%d-0", i)
			cletest.pollForLease(ctx, leaseName, "default", ptr.To(expectedWinner))
		}
		elapsed := time.Since(start)
		t.Logf("CLE: %d leases (x%d candidates) acquired in %v", numLeases, candidatesPerLease, elapsed)
	})
}

// TestCLEGracefulFailover measures failover time when the leader shuts down gracefully.
// With PR #138067, the LeaseCandidate is deleted on shutdown, so the server can
// re-elect immediately without waiting for GC.
func TestCLEGracefulFailover(t *testing.T) {
	// NOTE on measurement asymmetry:
	// Plain LE uses client-side expiry: isLeaseValid checks observedTime (when the CLIENT
	// last saw a record change) + LeaseDuration. Even if we force-expire on the server,
	// the acquiring client resets its observation clock on every Get, so it always waits a
	// full LeaseDuration from its own observation before acquiring.
	// CLE uses server-side expiry: the server checks the lease's actual RenewTime, so
	// force-expiring the lease lets the server re-elect immediately.
	// This means force-expire helps CLE but not plain LE. Both tests cancel the leader
	// and force-expire for consistency, but the plain client will still wait ~LeaseDuration.

	// --- Plain lease failover ---
	t.Run("plain", func(t *testing.T) {
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, genericfeatures.CoordinatedLeaderElection, false)

		server, err := apiservertesting.StartTestServer(t, apiservertesting.NewDefaultTestServerOptions(), nil, framework.SharedEtcd())
		if err != nil {
			t.Fatal(err)
		}
		defer server.TearDownFn()

		clientset := kubernetes.NewForConfigOrDie(server.ClientConfig)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cletest := setupCLE(server.ClientConfig, ctx, t)
		defer cletest.cleanup()

		// Start two controllers; first one acquires.
		go cletest.createAndRunFakeLegacyController("plain-fo-1", "default", "fo-plain")
		cletest.pollForLease(ctx, "fo-plain", "default", ptr.To("plain-fo-1"))

		go cletest.createAndRunFakeLegacyController("plain-fo-2", "default", "fo-plain")
		// Give the second controller time to start its acquire loop.
		time.Sleep(2 * time.Second)

		// Cancel leader and force-expire.
		cletest.cancelController("plain-fo-1", "default")
		forceExpireLease(ctx, t, clientset, "default", "fo-plain")

		// Plain client still waits ~LeaseDuration from its next observation (see note above).
		failoverStart := time.Now()
		cletest.pollForLease(ctx, "fo-plain", "default", ptr.To("plain-fo-2"))
		failoverTime := time.Since(failoverStart)
		t.Logf("Plain graceful failover: %v (includes client-side LeaseDuration wait)", failoverTime)
	})

	// --- CLE failover ---
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

		cletest := setupCLE(server.ClientConfig, ctx, t)
		defer cletest.cleanup()

		go cletest.createAndRunFakeController("cle-fo-1", "default", "fo-cle", "1.20.0", "1.20.0", v1.OldestEmulationVersion)
		cletest.pollForLease(ctx, "fo-cle", "default", ptr.To("cle-fo-1"))

		go cletest.createAndRunFakeController("cle-fo-2", "default", "fo-cle", "1.20.0", "1.20.0", v1.OldestEmulationVersion)
		pollForLeaseCandidate(ctx, t, clientset, "default", "cle-fo-2")

		// Graceful shutdown — LeaseCandidate gets deleted via #138067.
		// Server checks actual RenewTime, so force-expire triggers immediate re-election.
		cletest.cancelController("cle-fo-1", "default")
		forceExpireLease(ctx, t, clientset, "default", "fo-cle")

		failoverStart := time.Now()
		cletest.pollForLease(ctx, "fo-cle", "default", ptr.To("cle-fo-2"))
		failoverTime := time.Since(failoverStart)
		t.Logf("CLE graceful failover: %v (server-side re-election)", failoverTime)
	})
}

// TestCLECrashFailover measures re-election time when the leader crashes (ungraceful).
// Same measurement asymmetry as TestCLEGracefulFailover applies — see note there.
func TestCLECrashFailover(t *testing.T) {
	// --- Plain lease crash failover ---
	t.Run("plain", func(t *testing.T) {
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, genericfeatures.CoordinatedLeaderElection, false)

		server, err := apiservertesting.StartTestServer(t, apiservertesting.NewDefaultTestServerOptions(), nil, framework.SharedEtcd())
		if err != nil {
			t.Fatal(err)
		}
		defer server.TearDownFn()

		clientset := kubernetes.NewForConfigOrDie(server.ClientConfig)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cletest := setupCLE(server.ClientConfig, ctx, t)
		defer cletest.cleanup()

		go cletest.createAndRunFakeLegacyController("plain-crash-1", "default", "crash-plain")
		cletest.pollForLease(ctx, "crash-plain", "default", ptr.To("plain-crash-1"))

		go cletest.createAndRunFakeLegacyController("plain-crash-2", "default", "crash-plain")
		time.Sleep(2 * time.Second)

		// Simulate crash: cancel without cleanup + force-expire the lease.
		cletest.cancelController("plain-crash-1", "default")
		forceExpireLease(ctx, t, clientset, "default", "crash-plain")

		reelectionStart := time.Now()
		cletest.pollForLease(ctx, "crash-plain", "default", ptr.To("plain-crash-2"))
		reelectionTime := time.Since(reelectionStart)
		t.Logf("Plain crash re-election: %v", reelectionTime)
	})

	// --- CLE crash failover ---
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

		cletest := setupCLE(server.ClientConfig, ctx, t)
		defer cletest.cleanup()

		go cletest.createAndRunFakeController("cle-crash-1", "default", "crash-cle", "1.20.0", "1.20.0", v1.OldestEmulationVersion)
		cletest.pollForLease(ctx, "crash-cle", "default", ptr.To("cle-crash-1"))

		go cletest.createAndRunFakeController("cle-crash-2", "default", "crash-cle", "1.20.0", "1.20.0", v1.OldestEmulationVersion)
		pollForLeaseCandidate(ctx, t, clientset, "default", "cle-crash-2")

		// Crash: cancel and force-expire (LeaseCandidate NOT deleted — simulates SIGKILL).
		cletest.cancelController("cle-crash-1", "default")
		forceExpireLease(ctx, t, clientset, "default", "crash-cle")

		reelectionStart := time.Now()
		cletest.pollForLease(ctx, "crash-cle", "default", ptr.To("cle-crash-2"))
		reelectionTime := time.Since(reelectionStart)
		t.Logf("CLE crash re-election: %v", reelectionTime)
	})
}

// TestCLEHandoffToBetterCandidate measures the time for CLE to hand off leadership
// to a better candidate when one joins.
func TestCLEHandoffToBetterCandidate(t *testing.T) {
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

	cletest := setupCLE(server.ClientConfig, ctx, t)
	defer cletest.cleanup()

	// Phase 1: Establish leader with emulation version 1.20.
	go cletest.createAndRunFakeController("handoff-1", "default", "handoff-lease", "1.20.0", "1.20.0", v1.OldestEmulationVersion)
	cletest.pollForLease(ctx, "handoff-lease", "default", ptr.To("handoff-1"))
	t.Logf("Phase 1: handoff-1 is leader (emulation 1.20)")

	// Phase 2: Add a better candidate with emulation version 1.19.
	handoffStart := time.Now()
	go cletest.createAndRunFakeController("handoff-2", "default", "handoff-lease", "1.20.0", "1.19.0", v1.OldestEmulationVersion)
	pollForLeaseCandidate(ctx, t, clientset, "default", "handoff-2")
	cletest.pollForLease(ctx, "handoff-lease", "default", ptr.To("handoff-2"))
	handoffTime := time.Since(handoffStart)
	t.Logf("Phase 2: handoff to handoff-2 (emulation 1.19) took %v", handoffTime)

	if handoffTime > 20*time.Second {
		t.Errorf("Handoff too slow: %v > 20s", handoffTime)
	}
}

// --- Likely path tests ---

// TestCLERollingUpgrade simulates a rolling upgrade where candidates with different
// versions join and leave, verifying correct leadership transitions.
func TestCLERollingUpgrade(t *testing.T) {
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

	cletest := setupCLE(server.ClientConfig, ctx, t)
	defer cletest.cleanup()

	// Phase 1: Start 3 replicas at v1.19. The oldest emulation version wins (all same, so
	// first to register). Wait for any of them to become leader.
	go cletest.createAndRunFakeController("upgrade-a", "default", "upgrade-lease", "1.19.0", "1.19.0", v1.OldestEmulationVersion)
	go cletest.createAndRunFakeController("upgrade-b", "default", "upgrade-lease", "1.19.0", "1.19.0", v1.OldestEmulationVersion)
	go cletest.createAndRunFakeController("upgrade-c", "default", "upgrade-lease", "1.19.0", "1.19.0", v1.OldestEmulationVersion)

	// Wait for any leader to be elected.
	var initialLeader string
	err = wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 20*time.Second, true, func(ctx context.Context) (bool, error) {
		lease, err := clientset.CoordinationV1().Leases("default").Get(ctx, "upgrade-lease", metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != "" {
			initialLeader = *lease.Spec.HolderIdentity
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("timeout waiting for initial leader")
	}
	t.Logf("Phase 1: initial leader is %s (all v1.19)", initialLeader)

	// Phase 2: Simulate rolling upgrade — remove the leader and add a v1.20 replacement.
	cletest.cancelController(initialLeader, "default")
	forceExpireLease(ctx, t, clientset, "default", "upgrade-lease")

	go cletest.createAndRunFakeController("upgrade-d", "default", "upgrade-lease", "1.20.0", "1.20.0", v1.OldestEmulationVersion)
	pollForLeaseCandidate(ctx, t, clientset, "default", "upgrade-d")

	// A v1.19 candidate should win (oldest emulation version).
	var newLeader string
	err = wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 20*time.Second, true, func(ctx context.Context) (bool, error) {
		lease, err := clientset.CoordinationV1().Leases("default").Get(ctx, "upgrade-lease", metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != "" && *lease.Spec.HolderIdentity != initialLeader {
			newLeader = *lease.Spec.HolderIdentity
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("timeout waiting for new leader after rolling upgrade")
	}
	t.Logf("Phase 2: new leader is %s after removing %s and adding upgrade-d (v1.20)", newLeader, initialLeader)

	// The new leader should NOT be the v1.20 candidate — a v1.19 should win.
	if newLeader == "upgrade-d" {
		t.Errorf("Expected a v1.19 candidate to be elected, but got v1.20 candidate %s", newLeader)
	}

	// Phase 3: Continue upgrade — remove remaining v1.19 candidates.
	remainingV19 := []string{"upgrade-a", "upgrade-b", "upgrade-c"}
	for _, name := range remainingV19 {
		if name != initialLeader {
			cletest.cancelController(name, "default")
		}
	}
	forceExpireLease(ctx, t, clientset, "default", "upgrade-lease")

	// Only upgrade-d (v1.20) should be left.
	cletest.pollForLease(ctx, "upgrade-lease", "default", ptr.To("upgrade-d"))
	t.Logf("Phase 3: upgrade-d (v1.20) is leader after all v1.19 removed")
}

// TestCLELeaderStabilityUnderLoad verifies that an established CLE leader maintains
// leadership while the apiserver handles high lease churn from node heartbeats.
func TestCLELeaderStabilityUnderLoad(t *testing.T) {
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

	// Start node lease churn first.
	const numNodeLeases = 500
	createNodeLeases(ctx, t, clientset, numNodeLeases)
	churnCtx, churnCancel := context.WithCancel(ctx)
	defer churnCancel()
	go simulateNodeLeaseRenewals(churnCtx, t, clientset, numNodeLeases)

	cletest := setupCLE(server.ClientConfig, ctx, t)
	defer cletest.cleanup()

	go cletest.createAndRunFakeController("stable-1", "default", "stability-lease", "1.20.0", "1.20.0", v1.OldestEmulationVersion)
	cletest.pollForLease(ctx, "stability-lease", "default", ptr.To("stable-1"))

	// Monitor that the leader stays stable for 30 seconds under load.
	var leaderChanges atomic.Int32
	monitorCtx, monitorCancel := context.WithTimeout(ctx, 30*time.Second)
	defer monitorCancel()

	go func() {
		lastHolder := "stable-1"
		for {
			select {
			case <-monitorCtx.Done():
				return
			case <-time.After(500 * time.Millisecond):
				lease, err := clientset.CoordinationV1().Leases("default").Get(ctx, "stability-lease", metav1.GetOptions{})
				if err != nil {
					continue
				}
				if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != lastHolder {
					leaderChanges.Add(1)
					lastHolder = *lease.Spec.HolderIdentity
				}
			}
		}
	}()

	<-monitorCtx.Done()
	changes := leaderChanges.Load()
	t.Logf("Leader changes during 30s under %d-node churn: %d", numNodeLeases, changes)
	if changes > 0 {
		t.Errorf("Leader should remain stable under load, but changed %d times", changes)
	}
}

// --- Scalability tests ---

// TestCLEElectionLatencyUnderNodeChurn measures how election latency degrades
// as node lease churn increases. This tests the CLE controller's single worker
// processing node lease updates (no-op reconciles) while real elections are needed.
func TestCLEElectionLatencyUnderNodeChurn(t *testing.T) {
	nodeCounts := []int{0, 1000, 3000, 5000}

	type result struct {
		nodes             int
		initialElection   time.Duration
		reelection        time.Duration
		handoff           time.Duration
		churnRateAchieved float64 // actual renewals/s observed
	}
	var results []result

	for _, numNodes := range nodeCounts {
		t.Run(fmt.Sprintf("nodes_%d", numNodes), func(t *testing.T) {
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

			// Create node leases and start churn before any elections.
			var churnRate atomic.Int64
			if numNodes > 0 {
				createNodeLeases(ctx, t, clientset, numNodes)
				churnCtx, churnCancel := context.WithCancel(ctx)
				defer churnCancel()
				// Use multiple goroutines for higher churn rates.
				numChurnWorkers := numNodes / 500
				if numChurnWorkers < 1 {
					numChurnWorkers = 1
				}
				for w := 0; w < numChurnWorkers; w++ {
					go simulateNodeLeaseRenewalsWithCounter(churnCtx, t, clientset, numNodes, &churnRate)
				}
				// Let churn stabilize.
				time.Sleep(2 * time.Second)
			}

			cletest := setupCLE(server.ClientConfig, ctx, t)
			defer cletest.cleanup()

			// Measure 1: Initial election.
			start := time.Now()
			go cletest.createAndRunFakeController("scale-1", "default", "scale-lease", "1.20.0", "1.20.0", v1.OldestEmulationVersion)
			cletest.pollForLease(ctx, "scale-lease", "default", ptr.To("scale-1"))
			initialElection := time.Since(start)
			t.Logf("Initial election: %v (nodes: %d)", initialElection, numNodes)

			// Measure 2: Re-election after leader loss.
			go cletest.createAndRunFakeController("scale-2", "default", "scale-lease", "1.20.0", "1.20.0", v1.OldestEmulationVersion)
			pollForLeaseCandidate(ctx, t, clientset, "default", "scale-2")

			cletest.cancelController("scale-1", "default")
			forceExpireLease(ctx, t, clientset, "default", "scale-lease")

			reelectionStart := time.Now()
			cletest.pollForLease(ctx, "scale-lease", "default", ptr.To("scale-2"))
			reelection := time.Since(reelectionStart)
			t.Logf("Re-election: %v", reelection)

			// Measure 3: Handoff to better candidate.
			// This involves: electionNeeded detects better candidate → ping/ack → set PreferredHolder →
			// current leader steps down → lease expires → new election. Can take 10-15s.
			handoffStart := time.Now()
			go cletest.createAndRunFakeController("scale-3", "default", "scale-lease", "1.20.0", "1.19.0", v1.OldestEmulationVersion)
			pollForLeaseCandidate(ctx, t, clientset, "default", "scale-3")
			err = wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 45*time.Second, true, func(ctx context.Context) (bool, error) {
				lease, err := clientset.CoordinationV1().Leases("default").Get(ctx, "scale-lease", metav1.GetOptions{})
				if err != nil {
					return false, nil
				}
				return lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity == "scale-3", nil
			})
			if err != nil {
				t.Fatalf("timeout waiting for handoff to scale-3")
			}
			handoff := time.Since(handoffStart)
			t.Logf("Handoff to better candidate: %v", handoff)

			// Snapshot churn rate.
			observedChurn := float64(churnRate.Load()) / time.Since(start).Seconds()

			r := result{
				nodes:             numNodes,
				initialElection:   initialElection,
				reelection:        reelection,
				handoff:           handoff,
				churnRateAchieved: observedChurn,
			}
			results = append(results, r)

			// Hard limit: elections should complete within 20s even under heavy churn.
			const maxLatency = 20 * time.Second
			if initialElection > maxLatency {
				t.Errorf("Initial election too slow: %v > %v", initialElection, maxLatency)
			}
			if reelection > maxLatency {
				t.Errorf("Re-election too slow: %v > %v", reelection, maxLatency)
			}
			if handoff > maxLatency {
				t.Errorf("Handoff too slow: %v > %v", handoff, maxLatency)
			}
		})
	}

	// Print summary table.
	t.Logf("\n=== CLE Election Latency vs Node Lease Churn ===")
	t.Logf("%-10s %10s %15s %15s %15s", "Nodes", "Churn/s", "Initial", "Re-election", "Handoff")
	t.Logf("%-10s %10s %15s %15s %15s", strings.Repeat("-", 10), strings.Repeat("-", 10), strings.Repeat("-", 15), strings.Repeat("-", 15), strings.Repeat("-", 15))
	for _, r := range results {
		t.Logf("%-10d %10.0f %15v %15v %15v",
			r.nodes, r.churnRateAchieved,
			r.initialElection.Round(time.Millisecond),
			r.reelection.Round(time.Millisecond),
			r.handoff.Round(time.Millisecond))
	}
}

// simulateNodeLeaseRenewalsWithCounter is like simulateNodeLeaseRenewals but
// increments an atomic counter for each successful renewal.
func simulateNodeLeaseRenewalsWithCounter(ctx context.Context, t *testing.T, clientset kubernetes.Interface, numLeases int, counter *atomic.Int64) {
	t.Helper()
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
			i++
			continue
		}
		lease.Spec.RenewTime = &metav1.MicroTime{Time: time.Now()}
		_, err = clientset.CoordinationV1().Leases(corev1.NamespaceNodeLease).Update(ctx, lease, metav1.UpdateOptions{})
		if err != nil {
			i++
			continue
		}
		counter.Add(1)
		i++

		// Pace to avoid overwhelming the test apiserver.
		err = wait.PollUntilContextTimeout(ctx, 2*time.Millisecond, time.Second, true, func(ctx context.Context) (bool, error) {
			return true, nil
		})
		if err != nil {
			return
		}
	}
}

// TestCLEConcurrentElectionsUnderLoad measures how many concurrent elections
// the CLE controller can handle while under node lease churn.
func TestCLEConcurrentElectionsUnderLoad(t *testing.T) {
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

	// Start heavy churn.
	const numNodeLeases = 3000
	createNodeLeases(ctx, t, clientset, numNodeLeases)
	churnCtx, churnCancel := context.WithCancel(ctx)
	defer churnCancel()
	for w := 0; w < 6; w++ {
		go simulateNodeLeaseRenewals(churnCtx, t, clientset, numNodeLeases)
	}
	time.Sleep(2 * time.Second)

	// Trigger 20 concurrent elections (simulating KCM + scheduler + custom controllers).
	const numLeases = 20
	cletest := setupCLE(server.ClientConfig, ctx, t)
	defer cletest.cleanup()

	start := time.Now()
	for i := 0; i < numLeases; i++ {
		leaseName := fmt.Sprintf("concurrent-%d", i)
		name := fmt.Sprintf("ctrl-%d", i)
		go cletest.createAndRunFakeController(name, "default", leaseName, "1.20.0", "1.20.0", v1.OldestEmulationVersion)
	}

	for i := 0; i < numLeases; i++ {
		leaseName := fmt.Sprintf("concurrent-%d", i)
		expectedWinner := fmt.Sprintf("ctrl-%d", i)
		cletest.pollForLease(ctx, leaseName, "default", ptr.To(expectedWinner))
	}
	elapsed := time.Since(start)
	t.Logf("All %d elections completed in %v under %d-node churn", numLeases, elapsed, numNodeLeases)

	if elapsed > 30*time.Second {
		t.Errorf("Concurrent elections too slow: %v > 30s", elapsed)
	}
}

// --- Benchmark summary test ---

// TestCLEPerformanceSummary runs all key scenarios back-to-back and prints a comparison table.
func TestCLEPerformanceSummary(t *testing.T) {
	type result struct {
		scenario string
		plain    time.Duration
		cle      time.Duration
	}
	var results []result

	// Scenario 1: Single lease acquisition
	t.Run("single_acquisition", func(t *testing.T) {
		var plainTime, cleTime time.Duration

		t.Run("plain", func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, genericfeatures.CoordinatedLeaderElection, false)
			server, err := apiservertesting.StartTestServer(t, apiservertesting.NewDefaultTestServerOptions(), nil, framework.SharedEtcd())
			if err != nil {
				t.Fatal(err)
			}
			defer server.TearDownFn()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cletest := setupCLE(server.ClientConfig, ctx, t)
			defer cletest.cleanup()

			start := time.Now()
			go cletest.createAndRunFakeLegacyController("summary-p-1", "default", "summary-plain")
			cletest.pollForLease(ctx, "summary-plain", "default", ptr.To("summary-p-1"))
			plainTime = time.Since(start)
		})

		t.Run("coordinated", func(t *testing.T) {
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

			start := time.Now()
			go cletest.createAndRunFakeController("summary-c-1", "default", "summary-cle", "1.20.0", "1.20.0", v1.OldestEmulationVersion)
			cletest.pollForLease(ctx, "summary-cle", "default", ptr.To("summary-c-1"))
			cleTime = time.Since(start)
		})

		results = append(results, result{"single acquisition", plainTime, cleTime})
	})

	// Scenario 2: Re-election after leader loss
	t.Run("reelection", func(t *testing.T) {
		var plainTime, cleTime time.Duration

		t.Run("plain", func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, genericfeatures.CoordinatedLeaderElection, false)
			server, err := apiservertesting.StartTestServer(t, apiservertesting.NewDefaultTestServerOptions(), nil, framework.SharedEtcd())
			if err != nil {
				t.Fatal(err)
			}
			defer server.TearDownFn()

			clientset := kubernetes.NewForConfigOrDie(server.ClientConfig)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cletest := setupCLE(server.ClientConfig, ctx, t)
			defer cletest.cleanup()

			go cletest.createAndRunFakeLegacyController("re-p-1", "default", "re-plain")
			cletest.pollForLease(ctx, "re-plain", "default", ptr.To("re-p-1"))
			go cletest.createAndRunFakeLegacyController("re-p-2", "default", "re-plain")
			time.Sleep(2 * time.Second)

			cletest.cancelController("re-p-1", "default")
			forceExpireLease(ctx, t, clientset, "default", "re-plain")
			start := time.Now()
			cletest.pollForLease(ctx, "re-plain", "default", ptr.To("re-p-2"))
			plainTime = time.Since(start)
		})

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
			cletest := setupCLE(server.ClientConfig, ctx, t)
			defer cletest.cleanup()

			go cletest.createAndRunFakeController("re-c-1", "default", "re-cle", "1.20.0", "1.20.0", v1.OldestEmulationVersion)
			cletest.pollForLease(ctx, "re-cle", "default", ptr.To("re-c-1"))
			go cletest.createAndRunFakeController("re-c-2", "default", "re-cle", "1.20.0", "1.20.0", v1.OldestEmulationVersion)
			pollForLeaseCandidate(ctx, t, clientset, "default", "re-c-2")

			cletest.cancelController("re-c-1", "default")
			forceExpireLease(ctx, t, clientset, "default", "re-cle")
			start := time.Now()
			cletest.pollForLease(ctx, "re-cle", "default", ptr.To("re-c-2"))
			cleTime = time.Since(start)
		})

		results = append(results, result{"re-election after loss", plainTime, cleTime})
	})

	// Print summary table.
	t.Logf("\n=== CLE Performance Parity Summary ===")
	t.Logf("%-30s %15s %15s %10s", "Scenario", "Plain", "CLE", "Ratio")
	t.Logf("%-30s %15s %15s %10s", strings.Repeat("-", 30), strings.Repeat("-", 15), strings.Repeat("-", 15), strings.Repeat("-", 10))
	for _, r := range results {
		ratio := float64(r.cle) / float64(r.plain)
		t.Logf("%-30s %15v %15v %9.2fx", r.scenario, r.plain.Round(time.Millisecond), r.cle.Round(time.Millisecond), ratio)
	}
}
