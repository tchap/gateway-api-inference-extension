package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/ext-proc/backend"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/ext-proc/handlers"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/ext-proc/internal/runnable"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/ext-proc/scheduling"
)

// ExtProcServerRunner provides methods to manage an external process server.
type ExtProcServerRunner struct {
	GrpcPort                         int
	TargetEndpointKey                string
	PoolName                         string
	PoolNamespace                    string
	RefreshPodsInterval              time.Duration
	RefreshMetricsInterval           time.Duration
	RefreshPrometheusMetricsInterval time.Duration
	Datastore                        *backend.K8sDatastore
	SecureServing                    bool
	CertPath                         string
}

// Default values for CLI flags in main
const (
	DefaultGrpcPort                         = 9002                             // default for --grpcPort
	DefaultTargetEndpointKey                = "x-gateway-destination-endpoint" // default for --targetEndpointKey
	DefaultPoolName                         = ""                               // required but no default
	DefaultPoolNamespace                    = "default"                        // default for --poolNamespace
	DefaultRefreshPodsInterval              = 10 * time.Second                 // default for --refreshPodsInterval
	DefaultRefreshMetricsInterval           = 50 * time.Millisecond            // default for --refreshMetricsInterval
	DefaultRefreshPrometheusMetricsInterval = 5 * time.Second                  // default for --refreshPrometheusMetricsInterval
	DefaultSecureServing                    = true                             // default for --secureServing
)

func NewDefaultExtProcServerRunner() *ExtProcServerRunner {
	return &ExtProcServerRunner{
		GrpcPort:                         DefaultGrpcPort,
		TargetEndpointKey:                DefaultTargetEndpointKey,
		PoolName:                         DefaultPoolName,
		PoolNamespace:                    DefaultPoolNamespace,
		RefreshPodsInterval:              DefaultRefreshPodsInterval,
		RefreshMetricsInterval:           DefaultRefreshMetricsInterval,
		RefreshPrometheusMetricsInterval: DefaultRefreshPrometheusMetricsInterval,
		SecureServing:                    DefaultSecureServing,
		// Datastore can be assigned later.
	}
}

// SetupWithManager sets up the runner with the given manager.
func (r *ExtProcServerRunner) SetupWithManager(mgr ctrl.Manager) error {
	// Create the controllers and register them with the manager
	if err := (&backend.InferencePoolReconciler{
		Datastore: r.Datastore,
		Scheme:    mgr.GetScheme(),
		Client:    mgr.GetClient(),
		PoolNamespacedName: types.NamespacedName{
			Name:      r.PoolName,
			Namespace: r.PoolNamespace,
		},
		Record: mgr.GetEventRecorderFor("InferencePool"),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed setting up InferencePoolReconciler: %w", err)
	}

	if err := (&backend.InferenceModelReconciler{
		Datastore: r.Datastore,
		Scheme:    mgr.GetScheme(),
		Client:    mgr.GetClient(),
		PoolNamespacedName: types.NamespacedName{
			Name:      r.PoolName,
			Namespace: r.PoolNamespace,
		},
		Record: mgr.GetEventRecorderFor("InferenceModel"),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed setting up InferenceModelReconciler: %w", err)
	}

	if err := (&backend.PodReconciler{
		Datastore: r.Datastore,
		Scheme:    mgr.GetScheme(),
		Client:    mgr.GetClient(),
		Record:    mgr.GetEventRecorderFor("pod"),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed setting up EndpointSliceReconciler: %v", err)
	}
	return nil
}

// AsRunnable returns a Runnable that can be used to start the ext-proc gRPC server.
// The runnable implements LeaderElectionRunnable with leader election disabled.
func (r *ExtProcServerRunner) AsRunnable(
	logger logr.Logger,
	podDatastore *backend.K8sDatastore,
	podMetricsClient backend.PodMetricsClient,
) manager.Runnable {
	return runnable.NoLeaderElection(manager.RunnableFunc(func(ctx context.Context) error {
		// Initialize backend provider
		pp := backend.NewProvider(podMetricsClient, podDatastore)
		if err := pp.Init(logger.WithName("provider"), r.RefreshPodsInterval, r.RefreshMetricsInterval, r.RefreshPrometheusMetricsInterval); err != nil {
			logger.Error(err, "Failed to initialize backend provider")
			return err
		}

		var srv *grpc.Server
		if r.SecureServing {
			var cert tls.Certificate
			var err error
			if r.CertPath != "" {
				cert, err = tls.LoadX509KeyPair(r.CertPath+"/tls.crt", r.CertPath+"/tls.key")
				if err != nil {
					logger.Error(err, "Failed to load certificate key pair",
						"publicKeyPath", r.CertPath+"/tls.crt", "privateKeyPath", r.CertPath+"/tls.key")
					return err
				}
			} else {
				// Create tls based credential.
				cert, err = createSelfSignedTLSCertificate(logger)
				if err != nil {
					// Logging handled in createSelfSignedTLSCertificate.
					return err
				}
			}

			creds := credentials.NewTLS(&tls.Config{
				Certificates: []tls.Certificate{cert},
			})
			// Init the server.
			srv = grpc.NewServer(grpc.Creds(creds))
		} else {
			srv = grpc.NewServer()
		}
		extProcPb.RegisterExternalProcessorServer(
			srv,
			handlers.NewServer(pp, scheduling.NewScheduler(pp), r.TargetEndpointKey, r.Datastore),
		)

		// Forward to the gRPC runnable.
		return runnable.GRPCServer("ext-proc", srv, r.GrpcPort).Start(ctx)
	}))
}

func createSelfSignedTLSCertificate(logger logr.Logger) (tls.Certificate, error) {
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		logger.Error(err, "Failed to create serial number for self-signed cert")
		return tls.Certificate{}, err
	}
	now := time.Now()
	notBefore := now.UTC()
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Inference Ext"},
		},
		NotBefore:             notBefore,
		NotAfter:              now.Add(time.Hour * 24 * 365 * 10).UTC(), // 10 years
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	priv, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		logger.Error(err, "Failed to generate key for self-signed cert")
		return tls.Certificate{}, err
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		logger.Error(err, "Failed to create self-signed certificate")
		return tls.Certificate{}, err
	}

	certBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})

	privBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		logger.Error(err, "Failed to marshal private key for self-signed certificate")
		return tls.Certificate{}, err
	}
	keyBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privBytes})

	return tls.X509KeyPair(certBytes, keyBytes)
}
