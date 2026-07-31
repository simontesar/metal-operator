package probemock

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/go-logr/logr"
	metalv1alpha1 "github.com/ironcore-dev/metal-operator/api/v1alpha1"
	"github.com/ironcore-dev/metal-operator/internal/probe"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type agentEntry struct {
	cancel     context.CancelFunc
	systemUUID string
}

// ProbeMocker manages one probe.Agent per ServerBootConfiguration.
type ProbeMocker struct {
	client client.Client
	log    logr.Logger

	registryURL           string
	duration              time.Duration
	registryClientTimeout time.Duration
	lldpSyncInterval      time.Duration
	lldpSyncDuration      time.Duration

	parentCtx context.Context

	mu     sync.Mutex
	agents map[types.NamespacedName]agentEntry
}

// NewProbeMocker creates a ProbeMocker for the controller runtime.
func NewProbeMocker(
	client client.Client,
	log logr.Logger,
	parentCtx context.Context,
	registryURL string,
	duration, registryClientTimeout, lldpSyncInterval, lldpSyncDuration time.Duration,
) *ProbeMocker {
	return &ProbeMocker{
		client:                client,
		log:                   log,
		parentCtx:             parentCtx,
		registryURL:           registryURL,
		duration:              duration,
		registryClientTimeout: registryClientTimeout,
		lldpSyncInterval:      lldpSyncInterval,
		lldpSyncDuration:      lldpSyncDuration,
		agents:                make(map[types.NamespacedName]agentEntry),
	}
}

// Start implements manager.Runnable and stops all agents on shutdown.
func (p *ProbeMocker) Start(ctx context.Context) error {
	<-ctx.Done()
	p.stopAll()
	return nil
}

// EnqueueRequestsForServer enqueues ServerBootConfigurations that reference
// the changed Server.
func (p *ProbeMocker) EnqueueRequestsForServer() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		server, ok := obj.(*metalv1alpha1.Server)
		if !ok {
			return nil
		}

		configList := &metalv1alpha1.ServerBootConfigurationList{}
		if err := p.client.List(ctx, configList); err != nil {
			p.log.Error(err, "Failed to list ServerBootConfigurations for Server watch",
				"server", client.ObjectKeyFromObject(server))
			return nil
		}

		var requests []reconcile.Request
		for i := range configList.Items {
			config := &configList.Items[i]
			if config.Spec.ServerRef.Name == server.Name {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Namespace: config.Namespace,
						Name:      config.Name,
					},
				})
			}
		}
		return requests
	})
}

// Reconcile handles a ServerBootConfiguration and manages its probe agent.
func (p *ProbeMocker) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	key := req.NamespacedName

	config := &metalv1alpha1.ServerBootConfiguration{}
	if err := p.client.Get(ctx, key, config); err != nil {
		if apierrors.IsNotFound(err) {
			p.stopAgent(key)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	if !config.DeletionTimestamp.IsZero() {
		log.Info("Stopping probe agent for deleting ServerBootConfiguration")
		p.stopAgent(key)
		return reconcile.Result{}, nil
	}

	server := &metalv1alpha1.Server{}
	serverKey := types.NamespacedName{Name: config.Spec.ServerRef.Name}
	if err := p.client.Get(ctx, serverKey, server); err != nil {
		return reconcile.Result{}, err
	}

	if !p.shouldRunAgent(server) {
		if p.agentRunning(key) {
			log.Info("Stopping probe agent; Server not ready for discovery",
				"serverState", server.Status.State,
			)
			p.stopAgent(key)
		}

		if server.Status.State == "" ||
			server.Status.State == metalv1alpha1.ServerStateInitial ||
			server.Status.State == metalv1alpha1.ServerStateDiscovery {
			return reconcile.Result{Requeue: true}, nil
		}
		return reconcile.Result{}, nil
	}

	p.ensureAgent(key, server.Spec.SystemUUID)

	if err := p.ensureBootConfigReady(ctx, config); err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func (p *ProbeMocker) shouldRunAgent(server *metalv1alpha1.Server) bool {
	return server.Status.State == metalv1alpha1.ServerStateDiscovery &&
		server.Spec.SystemUUID != ""
}

func (p *ProbeMocker) ensureBootConfigReady(
	ctx context.Context,
	config *metalv1alpha1.ServerBootConfiguration,
) error {
	if config.Status.State == metalv1alpha1.ServerBootConfigurationStateReady {
		return nil
	}

	base := config.DeepCopy()
	config.Status.State = metalv1alpha1.ServerBootConfigurationStateReady
	if err := p.client.Status().Patch(ctx, config, client.MergeFrom(base)); err != nil {
		return err
	}

	ctrl.LoggerFrom(ctx).Info("Marked ServerBootConfiguration Ready",
		"serverBootConfiguration", config.Name,
		"namespace", config.Namespace,
	)
	return nil
}

func (p *ProbeMocker) agentRunning(key types.NamespacedName) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.agents[key]
	return ok
}

func (p *ProbeMocker) ensureAgent(key types.NamespacedName, systemUUID string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if entry, ok := p.agents[key]; ok && entry.systemUUID == systemUUID {
		return
	}

	if entry, ok := p.agents[key]; ok {
		entry.cancel()
		delete(p.agents, key)
	}

	agentCtx, cancel := context.WithCancel(p.parentCtx)
	agent := probe.NewAgent(
		p.log.WithValues(
			"serverBootConfiguration", key.Name,
			"namespace", key.Namespace,
			"systemUUID", systemUUID,
		),
		systemUUID,
		p.registryURL,
		p.duration,
		p.registryClientTimeout,
		p.lldpSyncInterval,
		p.lldpSyncDuration,
	)

	p.agents[key] = agentEntry{
		cancel:     cancel,
		systemUUID: systemUUID,
	}

	go func() {
		if err := agent.Start(agentCtx); err != nil &&
			!errors.Is(err, context.Canceled) {
			p.log.Error(err, "Probe agent stopped with error",
				"serverBootConfiguration", key.Name,
				"namespace", key.Namespace,
				"systemUUID", systemUUID,
			)
		}
	}()

	p.log.Info("Started probe agent",
		"serverBootConfiguration", key.Name,
		"namespace", key.Namespace,
		"systemUUID", systemUUID,
	)
}

func (p *ProbeMocker) stopAgent(key types.NamespacedName) {
	p.mu.Lock()
	defer p.mu.Unlock()

	entry, ok := p.agents[key]
	if !ok {
		return
	}

	entry.cancel()
	delete(p.agents, key)

	p.log.Info("Stopped probe agent",
		"serverBootConfiguration", key.Name,
		"namespace", key.Namespace,
		"systemUUID", entry.systemUUID,
	)
}

func (p *ProbeMocker) stopAll() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for key, entry := range p.agents {
		entry.cancel()
		p.log.Info("Stopped probe agent on shutdown",
			"serverBootConfiguration", key.Name,
			"namespace", key.Namespace,
			"systemUUID", entry.systemUUID,
		)
	}
	p.agents = make(map[types.NamespacedName]agentEntry)
}
