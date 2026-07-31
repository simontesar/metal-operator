package probemock_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metalv1alpha1 "github.com/ironcore-dev/metal-operator/api/v1alpha1"
	"github.com/ironcore-dev/metal-operator/cmd/metalprobe-mock-controller/probemock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const testSystemUUID = "38947555-7742-3448-3784-823347823834"

type registrationPayload struct {
	SystemUUID string `json:"systemUUID"`
}

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := metalv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return scheme
}

func newRegistryServer(t *testing.T, registerCount *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/register" {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read body: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			var payload registrationPayload
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Errorf("unmarshal payload: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if payload.SystemUUID != testSystemUUID {
				t.Errorf("unexpected system UUID: %s", payload.SystemUUID)
			}
			registerCount.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
}

func testObjects() (*metalv1alpha1.ServerBootConfiguration, *metalv1alpha1.Server) {
	server := &metalv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-server",
		},
		Spec: metalv1alpha1.ServerSpec{
			SystemUUID: testSystemUUID,
		},
		Status: metalv1alpha1.ServerStatus{
			State: metalv1alpha1.ServerStateDiscovery,
		},
	}
	config := &metalv1alpha1.ServerBootConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-boot-config",
			Namespace: "default",
		},
		Spec: metalv1alpha1.ServerBootConfigurationSpec{
			ServerRef: corev1.LocalObjectReference{
				Name: server.Name,
			},
		},
		Status: metalv1alpha1.ServerBootConfigurationStatus{
			State: metalv1alpha1.ServerBootConfigurationStatePending,
		},
	}
	return config, server
}

func newProbeMocker(
	t *testing.T,
	c client.Client,
	registryURL string,
) *probemock.ProbeMocker {
	t.Helper()
	return probemock.NewProbeMocker(
		c,
		logr.Discard(),
		context.Background(),
		registryURL,
		50*time.Millisecond,
		time.Second,
		50*time.Millisecond,
		250*time.Millisecond,
	)
}

func newFakeClient(t *testing.T, scheme *runtime.Scheme, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&metalv1alpha1.Server{}, &metalv1alpha1.ServerBootConfiguration{}).
		WithObjects(objs...).
		Build()
}

func TestProbeMocker_StartsAgentWhenDiscoveryPending(t *testing.T) {
	var registerCount atomic.Int32
	regServer := newRegistryServer(t, &registerCount)
	defer regServer.Close()

	scheme := newTestScheme(t)
	config, serverObj := testObjects()
	c := newFakeClient(t, scheme, config, serverObj)

	mocker := newProbeMocker(t, c, regServer.URL)
	key := types.NamespacedName{Namespace: config.Namespace, Name: config.Name}

	if _, err := mocker.Reconcile(context.Background(), reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	updated := &metalv1alpha1.ServerBootConfiguration{}
	if err := c.Get(context.Background(), key, updated); err != nil {
		t.Fatalf("get config: %v", err)
	}
	if updated.Status.State != metalv1alpha1.ServerBootConfigurationStateReady {
		t.Fatalf("expected SBC Ready, got %q", updated.Status.State)
	}

	waitForRegisters(t, &registerCount, 1)
}

func TestProbeMocker_StopsAgentOnDelete(t *testing.T) {
	var registerCount atomic.Int32
	regServer := newRegistryServer(t, &registerCount)
	defer regServer.Close()

	scheme := newTestScheme(t)
	config, serverObj := testObjects()
	c := newFakeClient(t, scheme, config, serverObj)

	mocker := newProbeMocker(t, c, regServer.URL)
	key := types.NamespacedName{Namespace: config.Namespace, Name: config.Name}

	if _, err := mocker.Reconcile(context.Background(), reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	waitForRegisters(t, &registerCount, 1)

	if err := c.Delete(context.Background(), config); err != nil {
		t.Fatalf("delete config: %v", err)
	}
	if _, err := mocker.Reconcile(context.Background(), reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile after delete: %v", err)
	}

	before := registerCount.Load()
	time.Sleep(200 * time.Millisecond)
	if registerCount.Load() != before {
		t.Fatalf("expected no further registrations after delete, got %d -> %d",
			before, registerCount.Load())
	}
}

func TestProbeMocker_StopsAgentWhenServerLeavesDiscovery(t *testing.T) {
	var registerCount atomic.Int32
	regServer := newRegistryServer(t, &registerCount)
	defer regServer.Close()

	scheme := newTestScheme(t)
	config, serverObj := testObjects()
	c := newFakeClient(t, scheme, config, serverObj)

	mocker := newProbeMocker(t, c, regServer.URL)
	key := types.NamespacedName{Namespace: config.Namespace, Name: config.Name}

	if _, err := mocker.Reconcile(context.Background(), reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	waitForRegisters(t, &registerCount, 1)

	serverObj.Status.State = metalv1alpha1.ServerStateAvailable
	if err := c.Status().Update(context.Background(), serverObj); err != nil {
		t.Fatalf("update server status: %v", err)
	}
	if _, err := mocker.Reconcile(context.Background(), reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile after server state change: %v", err)
	}

	before := registerCount.Load()
	time.Sleep(200 * time.Millisecond)
	if registerCount.Load() != before {
		t.Fatalf("expected no further registrations after leaving discovery")
	}
}

func TestProbeMocker_IdempotentReconcile(t *testing.T) {
	var registerCount atomic.Int32
	regServer := newRegistryServer(t, &registerCount)
	defer regServer.Close()

	scheme := newTestScheme(t)
	config, serverObj := testObjects()
	c := newFakeClient(t, scheme, config, serverObj)

	mocker := newProbeMocker(t, c, regServer.URL)
	key := types.NamespacedName{Namespace: config.Namespace, Name: config.Name}

	for range 3 {
		if _, err := mocker.Reconcile(context.Background(), reconcile.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}

	waitForRegisters(t, &registerCount, 1)
	time.Sleep(200 * time.Millisecond)
	if registerCount.Load() != 1 {
		t.Fatalf("expected a single initial registration, got %d", registerCount.Load())
	}
}

func waitForRegisters(t *testing.T, count *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if count.Load() >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d registrations, got %d", want, count.Load())
}
