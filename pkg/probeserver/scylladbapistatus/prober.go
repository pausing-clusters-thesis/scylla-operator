package scylladbapistatus

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"time"

	"github.com/scylladb/scylla-operator/pkg/controllerhelpers"
	"github.com/scylladb/scylla-operator/pkg/naming"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	corev1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
)

const (
	localhost = "localhost"
)

type Prober struct {
	namespace     string
	serviceName   string
	serviceLister corev1.ServiceLister
	timeout       time.Duration

	awaitPaths []string
}

func NewProber(
	namespace string,
	serviceName string,
	serviceLister corev1.ServiceLister,
	awaitPaths []string,
) *Prober {
	return &Prober{
		namespace:     namespace,
		serviceName:   serviceName,
		serviceLister: serviceLister,
		timeout:       60 * time.Second,

		awaitPaths: awaitPaths,
	}
}

func (p *Prober) serviceRef() string {
	return fmt.Sprintf("%s/%s", p.namespace, p.serviceName)
}

func (p *Prober) isNodeUnderMaintenance() (bool, error) {
	svc, err := p.serviceLister.Services(p.namespace).Get(p.serviceName)
	if err != nil {
		return false, err
	}

	_, hasLabel := svc.Labels[naming.NodeMaintenanceLabel]
	return hasLabel, nil
}

func (p *Prober) awaitPathsExist() (bool, error) {
	var err error
	var errs []error

	ready := true
	for _, path := range p.awaitPaths {
		_, err = os.Stat(path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, fmt.Errorf("can't stat path %q: %w", path, err))
				continue
			}

			ready = false
		}
	}

	err = utilerrors.NewAggregate(errs)
	if err != nil {
		return false, err
	}

	return ready, nil
}

func (p *Prober) Readyz(w http.ResponseWriter, req *http.Request) {
	ctx, ctxCancel := context.WithTimeout(req.Context(), p.timeout)
	defer ctxCancel()

	awaitPathsExist, err := p.awaitPathsExist()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		klog.ErrorS(err, "readyz probe: can't check required paths' existence")
		return
	}

	if !awaitPathsExist {
		w.WriteHeader(http.StatusServiceUnavailable)
		klog.V(2).InfoS("readyz probe: node is awaiting required paths' existence", "AwaitPaths", p.awaitPaths)
		return
	}

	underMaintenance, err := p.isNodeUnderMaintenance()
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		klog.ErrorS(err, "readyz probe: can't look up service maintenance label", "Service", p.serviceRef())
		return
	}

	if underMaintenance {
		// During maintenance Pod shouldn't be declare to be ready.
		w.WriteHeader(http.StatusServiceUnavailable)
		klog.V(2).InfoS("readyz probe: node is under maintenance", "Service", p.serviceRef())
		return
	}

	scyllaClient, err := controllerhelpers.NewScyllaClientForLocalhost()
	if err != nil {
		klog.ErrorS(err, "readyz probe: can't get scylla client", "Service", p.serviceRef())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer scyllaClient.Close()

	// Contact Scylla to learn about the status of the member
	nodeStatuses, err := scyllaClient.Status(ctx, localhost)
	if err != nil {
		klog.ErrorS(err, "readyz probe: can't get scylla node status", "Service", p.serviceRef())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	hostID, err := scyllaClient.GetLocalHostId(ctx, localhost, false)
	if err != nil {
		klog.ErrorS(err, "readyz probe: can't get host id")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	for _, s := range nodeStatuses {
		klog.V(4).InfoS("readyz probe: node state", "Node", s.Addr, "Status", s.Status, "State", s.State)

		if s.HostID == hostID && s.IsUN() {
			transportEnabled, err := scyllaClient.IsNativeTransportEnabled(ctx, localhost)
			if err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				klog.ErrorS(err, "readyz probe: can't get scylla native transport", "Service", p.serviceRef(), "Node", s.Addr)
				return
			}

			klog.V(4).InfoS("readyz probe: node state", "Node", s.Addr, "NativeTransportEnabled", transportEnabled)
			if transportEnabled {
				w.WriteHeader(http.StatusOK)
				return
			}
		}
	}

	klog.V(2).InfoS("readyz probe: node is not ready", "Service", p.serviceRef())
	w.WriteHeader(http.StatusServiceUnavailable)
}

func (p *Prober) Healthz(w http.ResponseWriter, req *http.Request) {
	ctx, ctxCancel := context.WithTimeout(req.Context(), p.timeout)
	defer ctxCancel()

	awaitPathsExist, err := p.awaitPathsExist()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		klog.ErrorS(err, "halthz probe: can't check required paths' existence")
		return
	}

	if !awaitPathsExist {
		w.WriteHeader(http.StatusOK)
		klog.V(2).InfoS("healthz probe: node is awaiting required paths' existence", "AwaitPaths", p.awaitPaths)
		return
	}

	underMaintenance, err := p.isNodeUnderMaintenance()
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		klog.ErrorS(err, "healthz probe: can't look up service maintenance label", "Service", p.serviceRef())
		return
	}

	if underMaintenance {
		w.WriteHeader(http.StatusOK)
		klog.V(2).InfoS("healthz probe: node is under maintenance", "Service", p.serviceRef())
		return
	}

	scyllaClient, err := controllerhelpers.NewScyllaClientForLocalhost()
	if err != nil {
		klog.ErrorS(err, "healthz probe: can't get scylla client", "Service", p.serviceRef())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer scyllaClient.Close()

	// Check if Scylla API is reachable
	_, err = scyllaClient.Ping(ctx, localhost)
	if err != nil {
		klog.ErrorS(err, "healthz probe: can't connect to Scylla API", "Service", p.serviceRef())
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	w.WriteHeader(http.StatusOK)
}
