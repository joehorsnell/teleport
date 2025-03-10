/*
Copyright 2015-2019 Gravitational, Inc.

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

// Package auth implements certificate signing authority and access control server
// Authority server is composed of several parts:
//
// * Authority server itself that implements signing and acl logic
// * HTTP server wrapper for authority server
// * HTTP client wrapper
package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	insecurerand "math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/oauth2"
	"github.com/google/uuid"
	liblicense "github.com/gravitational/license"
	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"golang.org/x/crypto/ssh"
	"golang.org/x/exp/slices"
	"golang.org/x/time/rate"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/api/client"
	"github.com/gravitational/teleport/api/client/proto"
	"github.com/gravitational/teleport/api/constants"
	apidefaults "github.com/gravitational/teleport/api/defaults"
	"github.com/gravitational/teleport/api/gen/proto/go/assist/v1"
	"github.com/gravitational/teleport/api/internalutils/stream"
	"github.com/gravitational/teleport/api/types"
	apievents "github.com/gravitational/teleport/api/types/events"
	"github.com/gravitational/teleport/api/types/wrappers"
	apiutils "github.com/gravitational/teleport/api/utils"
	"github.com/gravitational/teleport/api/utils/keys"
	"github.com/gravitational/teleport/api/utils/retryutils"
	apisshutils "github.com/gravitational/teleport/api/utils/sshutils"
	"github.com/gravitational/teleport/lib/ai"
	"github.com/gravitational/teleport/lib/ai/embedding"
	"github.com/gravitational/teleport/lib/auth/keystore"
	"github.com/gravitational/teleport/lib/auth/native"
	"github.com/gravitational/teleport/lib/auth/userloginstate"
	wanlib "github.com/gravitational/teleport/lib/auth/webauthn"
	wantypes "github.com/gravitational/teleport/lib/auth/webauthntypes"
	"github.com/gravitational/teleport/lib/authz"
	"github.com/gravitational/teleport/lib/backend"
	"github.com/gravitational/teleport/lib/circleci"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/gcp"
	"github.com/gravitational/teleport/lib/githubactions"
	"github.com/gravitational/teleport/lib/gitlab"
	"github.com/gravitational/teleport/lib/inventory"
	kubeutils "github.com/gravitational/teleport/lib/kube/utils"
	"github.com/gravitational/teleport/lib/kubernetestoken"
	"github.com/gravitational/teleport/lib/limiter"
	"github.com/gravitational/teleport/lib/loginrule"
	"github.com/gravitational/teleport/lib/modules"
	"github.com/gravitational/teleport/lib/observability/metrics"
	"github.com/gravitational/teleport/lib/observability/tracing"
	"github.com/gravitational/teleport/lib/release"
	"github.com/gravitational/teleport/lib/resourceusage"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/services/local"
	"github.com/gravitational/teleport/lib/srv/db/common/role"
	"github.com/gravitational/teleport/lib/sshca"
	"github.com/gravitational/teleport/lib/sshutils"
	"github.com/gravitational/teleport/lib/tlsca"
	usagereporter "github.com/gravitational/teleport/lib/usagereporter/teleport"
	"github.com/gravitational/teleport/lib/utils"
	"github.com/gravitational/teleport/lib/utils/interval"
	vc "github.com/gravitational/teleport/lib/versioncontrol"
	"github.com/gravitational/teleport/lib/versioncontrol/github"
	uw "github.com/gravitational/teleport/lib/versioncontrol/upgradewindow"
)

const (
	ErrFieldKeyUserMaxedAttempts = "maxed-attempts"

	// MaxFailedAttemptsErrMsg is a user friendly error message that tells a user that they are locked.
	MaxFailedAttemptsErrMsg = "too many incorrect attempts, please try again later"
)

const (
	// githubCacheTimeout is how long Github org entries are cached.
	githubCacheTimeout = time.Hour

	// mfaDeviceNameMaxLen is the maximum length of a device name.
	mfaDeviceNameMaxLen = 30
)

const (
	OSSDesktopsCheckPeriod  = 5 * time.Minute
	OSSDesktopsAlertID      = "oss-desktops"
	OSSDesktopsAlertMessage = "Your cluster is beyond its allocation of 5 non-Active Directory Windows desktops. " +
		"Reach out for unlimited desktops with Teleport Enterprise."

	OSSDesktopAlertLink = "https://goteleport.com/r/upgrade-community?utm_campaign=CTA_windows_local"
	OSSDesktopsLimit    = 5
)

var ErrRequiresEnterprise = services.ErrRequiresEnterprise

// ServerOption allows setting options as functional arguments to Server
type ServerOption func(*Server) error

// NewServer creates and configures a new Server instance
func NewServer(cfg *InitConfig, opts ...ServerOption) (*Server, error) {
	err := metrics.RegisterPrometheusCollectors(prometheusCollectors...)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if cfg.Trust == nil {
		cfg.Trust = local.NewCAService(cfg.Backend)
	}
	if cfg.Presence == nil {
		cfg.Presence = local.NewPresenceService(cfg.Backend)
	}
	if cfg.Provisioner == nil {
		cfg.Provisioner = local.NewProvisioningService(cfg.Backend)
	}
	if cfg.Identity == nil {
		cfg.Identity = local.NewIdentityService(cfg.Backend)
	}
	if cfg.Access == nil {
		cfg.Access = local.NewAccessService(cfg.Backend)
	}
	if cfg.DynamicAccessExt == nil {
		cfg.DynamicAccessExt = local.NewDynamicAccessService(cfg.Backend)
	}
	if cfg.ClusterConfiguration == nil {
		clusterConfig, err := local.NewClusterConfigurationService(cfg.Backend)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		cfg.ClusterConfiguration = clusterConfig
	}
	if cfg.Restrictions == nil {
		cfg.Restrictions = local.NewRestrictionsService(cfg.Backend)
	}
	if cfg.Apps == nil {
		cfg.Apps = local.NewAppService(cfg.Backend)
	}
	if cfg.Databases == nil {
		cfg.Databases = local.NewDatabasesService(cfg.Backend)
	}
	if cfg.DatabaseServices == nil {
		cfg.DatabaseServices = local.NewDatabaseServicesService(cfg.Backend)
	}
	if cfg.Kubernetes == nil {
		cfg.Kubernetes = local.NewKubernetesService(cfg.Backend)
	}
	if cfg.Status == nil {
		cfg.Status = local.NewStatusService(cfg.Backend)
	}
	if cfg.Assist == nil {
		cfg.Assist = local.NewAssistService(cfg.Backend)
	}
	if cfg.Events == nil {
		cfg.Events = local.NewEventsService(cfg.Backend)
	}
	if cfg.AuditLog == nil {
		cfg.AuditLog = events.NewDiscardAuditLog()
	}
	if cfg.Emitter == nil {
		cfg.Emitter = events.NewDiscardEmitter()
	}
	if cfg.Streamer == nil {
		cfg.Streamer = events.NewDiscardStreamer()
	}
	if cfg.WindowsDesktops == nil {
		cfg.WindowsDesktops = local.NewWindowsDesktopService(cfg.Backend)
	}
	if cfg.SAMLIdPServiceProviders == nil {
		cfg.SAMLIdPServiceProviders, err = local.NewSAMLIdPServiceProviderService(cfg.Backend)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}
	if cfg.UserGroups == nil {
		cfg.UserGroups, err = local.NewUserGroupService(cfg.Backend)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}
	if cfg.ConnectionsDiagnostic == nil {
		cfg.ConnectionsDiagnostic = local.NewConnectionsDiagnosticService(cfg.Backend)
	}
	if cfg.SessionTrackerService == nil {
		cfg.SessionTrackerService, err = local.NewSessionTrackerService(cfg.Backend)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}
	if cfg.AssertionReplayService == nil {
		cfg.AssertionReplayService = local.NewAssertionReplayService(cfg.Backend)
	}
	if cfg.TraceClient == nil {
		cfg.TraceClient = tracing.NewNoopClient()
	}
	if cfg.UsageReporter == nil {
		cfg.UsageReporter = usagereporter.DiscardUsageReporter{}
	}
	if cfg.Okta == nil {
		cfg.Okta, err = local.NewOktaService(cfg.Backend, cfg.Clock)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}
	if cfg.AccessLists == nil {
		cfg.AccessLists, err = local.NewAccessListService(cfg.Backend, cfg.Clock)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}
	if cfg.Integrations == nil {
		cfg.Integrations, err = local.NewIntegrationsService(cfg.Backend)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}
	if cfg.Embeddings == nil {
		cfg.Embeddings = local.NewEmbeddingsService(cfg.Backend)
	}
	if cfg.UserPreferences == nil {
		cfg.UserPreferences = local.NewUserPreferencesService(cfg.Backend)
	}
	if cfg.UserLoginState == nil {
		cfg.UserLoginState, err = local.NewUserLoginStateService(cfg.Backend)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}

	limiter, err := limiter.NewConnectionsLimiter(limiter.Config{
		MaxConnections: defaults.LimiterMaxConcurrentSignatures,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if cfg.KeyStoreConfig.PKCS11 != (keystore.PKCS11Config{}) {
		if !modules.GetModules().Features().HSM {
			return nil, fmt.Errorf("PKCS11 HSM support requires a license with the HSM feature enabled: %w", ErrRequiresEnterprise)
		}
		cfg.KeyStoreConfig.PKCS11.HostUUID = cfg.HostUUID
	} else if cfg.KeyStoreConfig.GCPKMS != (keystore.GCPKMSConfig{}) {
		if !modules.GetModules().Features().HSM {
			return nil, fmt.Errorf("Google Cloud KMS support requires a license with the HSM feature enabled: %w", ErrRequiresEnterprise)
		}
		cfg.KeyStoreConfig.GCPKMS.HostUUID = cfg.HostUUID
	} else {
		native.PrecomputeKeys()
		cfg.KeyStoreConfig.Software.RSAKeyPairSource = native.GenerateKeyPair
	}
	cfg.KeyStoreConfig.Logger = log
	keyStore, err := keystore.NewManager(context.Background(), cfg.KeyStoreConfig)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	services := &Services{
		Trust:                   cfg.Trust,
		PresenceInternal:        cfg.Presence,
		Provisioner:             cfg.Provisioner,
		Identity:                cfg.Identity,
		Access:                  cfg.Access,
		DynamicAccessExt:        cfg.DynamicAccessExt,
		ClusterConfiguration:    cfg.ClusterConfiguration,
		Restrictions:            cfg.Restrictions,
		Apps:                    cfg.Apps,
		Kubernetes:              cfg.Kubernetes,
		Databases:               cfg.Databases,
		DatabaseServices:        cfg.DatabaseServices,
		AuditLogSessionStreamer: cfg.AuditLog,
		Events:                  cfg.Events,
		WindowsDesktops:         cfg.WindowsDesktops,
		SAMLIdPServiceProviders: cfg.SAMLIdPServiceProviders,
		UserGroups:              cfg.UserGroups,
		SessionTrackerService:   cfg.SessionTrackerService,
		ConnectionsDiagnostic:   cfg.ConnectionsDiagnostic,
		Integrations:            cfg.Integrations,
		Embeddings:              cfg.Embeddings,
		Okta:                    cfg.Okta,
		AccessLists:             cfg.AccessLists,
		UserLoginStates:         cfg.UserLoginState,
		StatusInternal:          cfg.Status,
		UsageReporter:           cfg.UsageReporter,
		Assistant:               cfg.Assist,
		UserPreferences:         cfg.UserPreferences,
	}

	closeCtx, cancelFunc := context.WithCancel(context.TODO())
	as := Server{
		bk:                  cfg.Backend,
		clock:               cfg.Clock,
		limiter:             limiter,
		Authority:           cfg.Authority,
		AuthServiceName:     cfg.AuthServiceName,
		ServerID:            cfg.HostUUID,
		githubClients:       make(map[string]*githubClient),
		cancelFunc:          cancelFunc,
		closeCtx:            closeCtx,
		emitter:             cfg.Emitter,
		Streamer:            cfg.Streamer,
		Unstable:            local.NewUnstableService(cfg.Backend, cfg.AssertionReplayService),
		Services:            services,
		Cache:               services,
		keyStore:            keyStore,
		traceClient:         cfg.TraceClient,
		fips:                cfg.FIPS,
		loadAllCAs:          cfg.LoadAllCAs,
		httpClientForAWSSTS: cfg.HTTPClientForAWSSTS,
		embeddingsRetriever: cfg.EmbeddingRetriever,
		embedder:            cfg.EmbeddingClient,
	}
	as.inventory = inventory.NewController(&as, services, inventory.WithAuthServerID(cfg.HostUUID))
	for _, o := range opts {
		if err := o(&as); err != nil {
			return nil, trace.Wrap(err)
		}
	}
	if as.clock == nil {
		as.clock = clockwork.NewRealClock()
	}
	as.githubOrgSSOCache, err = utils.NewFnCache(utils.FnCacheConfig{
		TTL: githubCacheTimeout,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	as.ttlCache, err = utils.NewFnCache(utils.FnCacheConfig{
		TTL: time.Second * 3,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if as.ghaIDTokenValidator == nil {
		as.ghaIDTokenValidator = githubactions.NewIDTokenValidator(
			githubactions.IDTokenValidatorConfig{
				Clock: as.clock,
			},
		)
	}
	if as.gitlabIDTokenValidator == nil {
		as.gitlabIDTokenValidator, err = gitlab.NewIDTokenValidator(
			gitlab.IDTokenValidatorConfig{
				Clock:             as.clock,
				ClusterNameGetter: services,
			},
		)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}
	if as.circleCITokenValidate == nil {
		as.circleCITokenValidate = func(
			ctx context.Context, organizationID, token string,
		) (*circleci.IDTokenClaims, error) {
			return circleci.ValidateToken(
				ctx, as.clock, circleci.IssuerURLTemplate, organizationID, token,
			)
		}
	}
	if as.kubernetesTokenValidator == nil {
		as.kubernetesTokenValidator = &kubernetestoken.Validator{}
	}

	if as.gcpIDTokenValidator == nil {
		as.gcpIDTokenValidator = gcp.NewIDTokenValidator(
			gcp.IDTokenValidatorConfig{
				Clock: as.clock,
			},
		)
	}

	// Add in a login hook for generating state during user login.
	ulsGenerator, err := userloginstate.NewGenerator(userloginstate.GeneratorConfig{
		Log:         log,
		AccessLists: services,
		Access:      services,
		Clock:       cfg.Clock,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	as.RegisterLoginHook(ulsGenerator.LoginHook(services.UserLoginStates))

	return &as, nil
}

type Services struct {
	services.Trust
	services.PresenceInternal
	services.Provisioner
	services.Identity
	services.Access
	services.DynamicAccessExt
	services.ClusterConfiguration
	services.Restrictions
	services.Apps
	services.Kubernetes
	services.Databases
	services.DatabaseServices
	services.WindowsDesktops
	services.SAMLIdPServiceProviders
	services.UserGroups
	services.SessionTrackerService
	services.ConnectionsDiagnostic
	services.StatusInternal
	services.Integrations
	services.Okta
	services.AccessLists
	services.UserLoginStates
	services.Assistant
	services.Embeddings
	services.UserPreferences
	usagereporter.UsageReporter
	types.Events
	events.AuditLogSessionStreamer
}

// GetWebSession returns existing web session described by req.
// Implements ReadAccessPoint
func (r *Services) GetWebSession(ctx context.Context, req types.GetWebSessionRequest) (types.WebSession, error) {
	return r.Identity.WebSessions().Get(ctx, req)
}

// GetWebToken returns existing web token described by req.
// Implements ReadAccessPoint
func (r *Services) GetWebToken(ctx context.Context, req types.GetWebTokenRequest) (types.WebToken, error) {
	return r.Identity.WebTokens().Get(ctx, req)
}

// OktaClient returns the okta client.
func (r *Services) OktaClient() services.Okta {
	return r
}

// AccessListClient returns the access list client.
func (r *Services) AccessListClient() services.AccessLists {
	return r
}

// UserLoginStateClient returns the user login state client.
func (r *Services) UserLoginStateClient() services.UserLoginStates {
	return r
}

var (
	generateRequestsCount = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: teleport.MetricGenerateRequests,
			Help: "Number of requests to generate new server keys",
		},
	)
	generateThrottledRequestsCount = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: teleport.MetricGenerateRequestsThrottled,
			Help: "Number of throttled requests to generate new server keys",
		},
	)
	generateRequestsCurrent = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: teleport.MetricGenerateRequestsCurrent,
			Help: "Number of current generate requests for server keys",
		},
	)
	generateRequestsLatencies = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name: teleport.MetricGenerateRequestsHistogram,
			Help: "Latency for generate requests for server keys",
			// lowest bucket start of upper bound 0.001 sec (1 ms) with factor 2
			// highest bucket start of 0.001 sec * 2^15 == 32.768 sec
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 16),
		},
	)
	// UserLoginCount counts user logins
	UserLoginCount = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: teleport.MetricUserLoginCount,
			Help: "Number of times there was a user login",
		},
	)

	heartbeatsMissedByAuth = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: teleport.MetricHeartbeatsMissed,
			Help: "Number of heartbeats missed by auth server",
		},
	)

	registeredAgents = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: teleport.MetricNamespace,
			Name:      teleport.MetricRegisteredServers,
			Help:      "The number of Teleport services that are connected to an auth server by version.",
		},
		[]string{teleport.TagVersion},
	)

	registeredAgentsInstallMethod = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: teleport.MetricNamespace,
			Name:      teleport.MetricRegisteredServersByInstallMethods,
			Help:      "The number of Teleport services that are connected to an auth server by install method.",
		},
		[]string{teleport.TagInstallMethods},
	)

	migrations = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: teleport.MetricNamespace,
			Name:      teleport.MetricMigrations,
			Help:      "Migrations tracks for each migration if it is active (1) or not (0).",
		},
		[]string{teleport.TagMigration},
	)

	totalInstancesMetric = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: teleport.MetricNamespace,
			Name:      teleport.MetricTotalInstances,
			Help:      "Total teleport instances",
		},
	)

	enrolledInUpgradesMetric = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: teleport.MetricNamespace,
			Name:      teleport.MetricEnrolledInUpgrades,
			Help:      "Number of instances enrolled in automatic upgrades",
		},
	)

	upgraderCountsMetric = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: teleport.MetricNamespace,
			Name:      teleport.MetricUpgraderCounts,
			Help:      "Tracks the number of instances advertising each upgrader",
		},
		[]string{teleport.TagUpgrader},
	)

	accessRequestsCreatedMetric = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: teleport.MetricNamespace,
			Name:      teleport.MetricAccessRequestsCreated,
			Help:      "Tracks the number of created access requests",
		},
		[]string{teleport.TagRoles, teleport.TagResources},
	)

	prometheusCollectors = []prometheus.Collector{
		generateRequestsCount, generateThrottledRequestsCount,
		generateRequestsCurrent, generateRequestsLatencies, UserLoginCount, heartbeatsMissedByAuth,
		registeredAgents, migrations,
		totalInstancesMetric, enrolledInUpgradesMetric, upgraderCountsMetric,
		accessRequestsCreatedMetric,
		registeredAgentsInstallMethod,
	}
)

// LoginHook is a function that will be called on a successful login. This will likely be used
// for enterprise services that need to add in feature specific operations after a user has been
// successfully authenticated. An example would be creating objects based on the user.
type LoginHook func(context.Context, types.User) error

// Server keeps the cluster together. It acts as a certificate authority (CA) for
// a cluster and:
//   - generates the keypair for the node it's running on
//   - invites other SSH nodes to a cluster, by issuing invite tokens
//   - adds other SSH nodes to a cluster, by checking their token and signing their keys
//   - same for users and their sessions
//   - checks public keys to see if they're signed by it (can be trusted or not)
type Server struct {
	lock          sync.RWMutex
	githubClients map[string]*githubClient
	clock         clockwork.Clock
	bk            backend.Backend

	closeCtx   context.Context
	cancelFunc context.CancelFunc

	samlAuthService SAMLService
	oidcAuthService OIDCService

	releaseService release.Client

	loginRuleEvaluator loginrule.Evaluator

	sshca.Authority

	upgradeWindowStartHourGetter func(context.Context) (int64, error)

	// AuthServiceName is a human-readable name of this CA. If several Auth services are running
	// (managing multiple teleport clusters) this field is used to tell them apart in UIs
	// It usually defaults to the hostname of the machine the Auth service runs on.
	AuthServiceName string

	// ServerID is the server ID of this auth server.
	ServerID string

	// Unstable implements Unstable backend methods not suitable
	// for inclusion in Services.
	Unstable local.UnstableService

	// Services encapsulate services - provisioner, trust, etc. used by the auth
	// server in a separate structure. Reads through Services hit the backend.
	*Services

	// Cache should either be the same as Services, or a caching layer over it.
	// As it's an interface (and thus directly implementing all of its methods)
	// its embedding takes priority over Services (which only indirectly
	// implements its methods), thus any implemented GetFoo method on both Cache
	// and Services will call the one from Cache. To bypass the cache, call the
	// method on Services instead.
	Cache

	// privateKey is used in tests to use pre-generated private keys
	privateKey []byte

	// cipherSuites is a list of ciphersuites that the auth server supports.
	cipherSuites []uint16

	// limiter limits the number of active connections per client IP.
	limiter *limiter.ConnectionsLimiter

	// Emitter is events emitter, used to submit discrete events
	emitter apievents.Emitter

	// Streamer is an events session streamer, used to create continuous
	// session related streams
	events.Streamer

	// keyStore manages all CA private keys, which  may or may not be backed by
	// HSMs
	keyStore *keystore.Manager

	// lockWatcher is a lock watcher, used to verify cert generation requests.
	lockWatcher *services.LockWatcher

	// UnifiedResourceCache is a cache of multiple resource kinds to be presented
	// in a unified manner in the web UI.
	UnifiedResourceCache *services.UnifiedResourceCache

	inventory *inventory.Controller

	// githubOrgSSOCache is used to cache whether Github organizations use
	// external SSO or not.
	githubOrgSSOCache *utils.FnCache

	// ttlCache is a generic ttl cache. typed keys must be used.
	ttlCache *utils.FnCache

	// traceClient is used to forward spans to the upstream collector for components
	// within the cluster that don't have a direct connection to said collector
	traceClient otlptrace.Client

	// fips means FedRAMP/FIPS 140-2 compliant configuration was requested.
	fips bool

	// ghaIDTokenValidator allows ID tokens from GitHub Actions to be validated
	// by the auth server. It can be overridden for the purpose of tests.
	ghaIDTokenValidator ghaIDTokenValidator

	// gitlabIDTokenValidator allows ID tokens from GitLab CI to be validated by
	// the auth server. It can be overridden for the purpose of tests.
	gitlabIDTokenValidator gitlabIDTokenValidator

	// circleCITokenValidate allows ID tokens from CircleCI to be validated by
	// the auth server. It can be overridden for the purpose of tests.
	circleCITokenValidate func(ctx context.Context, organizationID, token string) (*circleci.IDTokenClaims, error)

	// kubernetesTokenValidator allows tokens from Kubernetes to be validated
	// by the auth server. It can be overridden for the purpose of tests.
	kubernetesTokenValidator kubernetesTokenValidator

	// gcpIDTokenValidator allows ID tokens from GCP to be validated by the auth
	// server. It can be overridden for the purpose of tests.
	gcpIDTokenValidator gcpIDTokenValidator

	// loadAllCAs tells tsh to load the host CAs for all clusters when trying to ssh into a node.
	loadAllCAs bool

	// license is the Teleport Enterprise license used to start the auth server
	license *liblicense.License

	// headlessAuthenticationWatcher is a headless authentication watcher,
	// used to catch and propagate headless authentication request changes.
	headlessAuthenticationWatcher *local.HeadlessAuthenticationWatcher

	loginHooksMu sync.RWMutex
	// loginHooks are a list of hooks that will be called on login.
	loginHooks []LoginHook

	// httpClientForAWSSTS overwrites the default HTTP client used for making
	// STS requests.
	httpClientForAWSSTS utils.HTTPDoClient

	// embeddingRetriever is a retriever used to retrieve embeddings from the backend.
	embeddingsRetriever *ai.SimpleRetriever

	// embedder is an embedder client used to generate embeddings.
	embedder embedding.Embedder
}

// SetSAMLService registers svc as the SAMLService that provides the SAML
// connector implementation. If a SAMLService has already been registered, this
// will override the previous registration.
func (a *Server) SetSAMLService(svc SAMLService) {
	a.samlAuthService = svc
}

// SetOIDCService registers svc as the OIDCService that provides the OIDC
// connector implementation. If a OIDCService has already been registered, this
// will override the previous registration.
func (a *Server) SetOIDCService(svc OIDCService) {
	a.oidcAuthService = svc
}

// SetLicense sets the license
func (a *Server) SetLicense(license *liblicense.License) {
	a.license = license
}

// SetReleaseService sets the release service
func (a *Server) SetReleaseService(svc release.Client) {
	a.releaseService = svc
}

// SetUpgradeWindowStartHourGetter sets the getter used to sync the ClusterMaintenanceConfig resource
// with the cloud UpgradeWindowStartHour value.
func (a *Server) SetUpgradeWindowStartHourGetter(fn func(context.Context) (int64, error)) {
	a.lock.Lock()
	defer a.lock.Unlock()
	a.upgradeWindowStartHourGetter = fn
}

func (a *Server) getUpgradeWindowStartHourGetter() func(context.Context) (int64, error) {
	a.lock.Lock()
	defer a.lock.Unlock()
	return a.upgradeWindowStartHourGetter
}

// SetLoginRuleEvaluator sets the login rule evaluator.
func (a *Server) SetLoginRuleEvaluator(l loginrule.Evaluator) {
	a.loginRuleEvaluator = l
}

// GetLoginRuleEvaluator returns the login rule evaluator. It is guaranteed not
// to return nil, if no evaluator has been installed it will return
// [loginrule.NullEvaluator].
func (a *Server) GetLoginRuleEvaluator() loginrule.Evaluator {
	if a.loginRuleEvaluator == nil {
		return loginrule.NullEvaluator{}
	}
	return a.loginRuleEvaluator
}

// RegisterLoginHook will register a login hook with the auth server.
func (a *Server) RegisterLoginHook(hook LoginHook) {
	a.loginHooksMu.Lock()
	defer a.loginHooksMu.Unlock()

	a.loginHooks = append(a.loginHooks, hook)
}

// CallLoginHooks will call the registered login hooks.
func (a *Server) CallLoginHooks(ctx context.Context, user types.User) error {
	// Make a copy of the login hooks to operate on.
	a.loginHooksMu.RLock()
	loginHooks := make([]LoginHook, len(a.loginHooks))
	copy(loginHooks, a.loginHooks)
	a.loginHooksMu.RUnlock()

	if len(loginHooks) == 0 {
		return nil
	}

	var errs []error
	for _, hook := range loginHooks {
		errs = append(errs, hook(ctx, user))
	}

	return trace.NewAggregate(errs...)
}

// ResetLoginHooks will clear out the login hooks.
func (a *Server) ResetLoginHooks() {
	a.loginHooksMu.Lock()
	a.loginHooks = nil
	a.loginHooksMu.Unlock()
}

// CloseContext returns the close context
func (a *Server) CloseContext() context.Context {
	return a.closeCtx
}

// SetUnifiedResourcesCache sets the unified resource cache.
func (a *Server) SetUnifiedResourcesCache(unifiedResourcesCache *services.UnifiedResourceCache) {
	a.lock.Lock()
	defer a.lock.Unlock()
	a.UnifiedResourceCache = unifiedResourcesCache
}

func (a *Server) SetLockWatcher(lockWatcher *services.LockWatcher) {
	a.lock.Lock()
	defer a.lock.Unlock()
	a.lockWatcher = lockWatcher
}

func (a *Server) checkLockInForce(mode constants.LockingMode, targets []types.LockTarget) error {
	a.lock.RLock()
	defer a.lock.RUnlock()
	if a.lockWatcher == nil {
		return trace.BadParameter("lockWatcher is not set")
	}
	return a.lockWatcher.CheckLockInForce(mode, targets...)
}

func (a *Server) SetHeadlessAuthenticationWatcher(headlessAuthenticationWatcher *local.HeadlessAuthenticationWatcher) {
	a.lock.Lock()
	defer a.lock.Unlock()
	a.headlessAuthenticationWatcher = headlessAuthenticationWatcher
}

// syncUpgradeWindowStartHour attempts to load the cloud UpgradeWindowStartHour value and set
// the ClusterMaintenanceConfig resource's AgentUpgrade.UTCStartHour field to match it.
func (a *Server) syncUpgradeWindowStartHour(ctx context.Context) error {
	getter := a.getUpgradeWindowStartHourGetter()
	if getter == nil {
		return trace.Errorf("getter has not been registered")
	}

	startHour, err := getter(ctx)
	if err != nil {
		return trace.Wrap(err)
	}

	cmc, err := a.GetClusterMaintenanceConfig(ctx)
	if err != nil {
		if !trace.IsNotFound(err) {
			return trace.Wrap(err)
		}

		// create an empty maintenance config resource on NotFound
		cmc = types.NewClusterMaintenanceConfig()
	}

	agentWindow, _ := cmc.GetAgentUpgradeWindow()

	agentWindow.UTCStartHour = uint32(startHour)

	cmc.SetAgentUpgradeWindow(agentWindow)

	if err := a.UpdateClusterMaintenanceConfig(ctx, cmc); err != nil {
		return trace.Wrap(err)
	}

	return nil
}

func (a *Server) periodicSyncUpgradeWindowStartHour() {
	checkInterval := interval.New(interval.Config{
		Duration:      time.Minute * 3,
		FirstDuration: utils.FullJitter(time.Second * 30),
		Jitter:        retryutils.NewSeventhJitter(),
	})
	defer checkInterval.Stop()

	for {
		select {
		case <-checkInterval.Next():
			if err := a.syncUpgradeWindowStartHour(a.closeCtx); err != nil {
				if a.closeCtx.Err() == nil {
					// we run this periodic at a fairly high frequency, so errors are just
					// logged but otherwise ignored.
					log.Warnf("Failed to sync upgrade window start hour: %v", err)
				}
			}
		case <-a.closeCtx.Done():
			return
		}
	}
}

// runPeriodicOperations runs some periodic bookkeeping operations
// performed by auth server
func (a *Server) runPeriodicOperations() {
	ctx := context.TODO()
	// run periodic functions with a semi-random period
	// to avoid contention on the database in case if there are multiple
	// auth servers running - so they don't compete trying
	// to update the same resources.
	r := insecurerand.New(insecurerand.NewSource(a.GetClock().Now().UnixNano()))
	period := defaults.HighResPollingPeriod + time.Duration(r.Intn(int(defaults.HighResPollingPeriod/time.Second)))*time.Second
	log.Debugf("Ticking with period: %v.", period)
	a.lock.RLock()
	ticker := a.clock.NewTicker(period)
	a.lock.RUnlock()
	// Create a ticker with jitter
	heartbeatCheckTicker := interval.New(interval.Config{
		Duration: apidefaults.ServerKeepAliveTTL() * 2,
		Jitter:   retryutils.NewSeventhJitter(),
	})
	promTicker := interval.New(interval.Config{
		Duration: defaults.PrometheusScrapeInterval,
		Jitter:   retryutils.NewSeventhJitter(),
	})
	missedKeepAliveCount := 0
	defer ticker.Stop()
	defer heartbeatCheckTicker.Stop()
	defer promTicker.Stop()

	firstReleaseCheck := utils.FullJitter(time.Hour * 6)

	// this environment variable is "unstable" since it will be deprecated
	// by an upcoming tctl command. currently exists for testing purposes only.
	if os.Getenv("TELEPORT_UNSTABLE_VC_SYNC_ON_START") == "yes" {
		firstReleaseCheck = utils.HalfJitter(time.Second * 10)
	}

	// note the use of FullJitter for the releases check interval. this lets us ensure
	// that frequent restarts don't prevent checks from happening despite the infrequent
	// effective check rate.
	releaseCheck := interval.New(interval.Config{
		Duration:      time.Hour * 24,
		FirstDuration: firstReleaseCheck,
		Jitter:        retryutils.NewFullJitter(),
	})
	defer releaseCheck.Stop()

	// more frequent release check that just re-calculates alerts based on previously
	// pulled versioning info.
	localReleaseCheck := interval.New(interval.Config{
		Duration:      time.Minute * 10,
		FirstDuration: utils.HalfJitter(time.Second * 10),
		Jitter:        retryutils.NewHalfJitter(),
	})
	defer localReleaseCheck.Stop()

	instancePeriodics := interval.New(interval.Config{
		Duration:      time.Minute * 9,
		FirstDuration: utils.HalfJitter(time.Minute),
		Jitter:        retryutils.NewSeventhJitter(),
	})
	defer instancePeriodics.Stop()

	var ossDesktopsCheck <-chan time.Time
	if modules.GetModules().BuildType() == modules.BuildOSS {
		ossDesktopsCheck = interval.New(interval.Config{
			Duration:      OSSDesktopsCheckPeriod,
			FirstDuration: utils.HalfJitter(time.Second * 10),
			Jitter:        retryutils.NewHalfJitter(),
		}).Next()
	} else if err := a.DeleteClusterAlert(ctx, OSSDesktopsAlertID); err != nil && !trace.IsNotFound(err) {
		log.Warnf("Can't delete OSS non-AD desktops limit alert: %v", err)
	}

	// isolate the schedule of potentially long-running refreshRemoteClusters() from other tasks
	go func() {
		// reasonably small interval to ensure that users observe clusters as online within 1 minute of adding them.
		remoteClustersRefresh := interval.New(interval.Config{
			Duration: time.Second * 40,
			Jitter:   retryutils.NewSeventhJitter(),
		})
		defer remoteClustersRefresh.Stop()

		for {
			select {
			case <-a.closeCtx.Done():
				return
			case <-remoteClustersRefresh.Next():
				a.refreshRemoteClusters(ctx, r)
			}
		}
	}()

	// cloud auth servers need to periodically sync the upgrade window
	// from the cloud db.
	if modules.GetModules().Features().Cloud {
		go a.periodicSyncUpgradeWindowStartHour()
	}

	for {
		select {
		case <-a.closeCtx.Done():
			return
		case <-ticker.Chan():
			err := a.autoRotateCertAuthorities(ctx)
			if err != nil {
				if trace.IsCompareFailed(err) {
					log.Debugf("Cert authority has been updated concurrently: %v.", err)
				} else {
					log.Errorf("Failed to perform cert rotation check: %v.", err)
				}
			}
		case <-heartbeatCheckTicker.Next():
			nodes, err := a.GetNodes(ctx, apidefaults.Namespace)
			if err != nil {
				log.Errorf("Failed to load nodes for heartbeat metric calculation: %v", err)
			}
			for _, node := range nodes {
				if services.NodeHasMissedKeepAlives(node) {
					missedKeepAliveCount++
				}
			}
			// Update prometheus gauge
			heartbeatsMissedByAuth.Set(float64(missedKeepAliveCount))
		case <-promTicker.Next():
			a.updateVersionMetrics()
			a.updateInstallMethodsMetrics()
		case <-releaseCheck.Next():
			a.syncReleaseAlerts(ctx, true)
		case <-localReleaseCheck.Next():
			a.syncReleaseAlerts(ctx, false)
		case <-instancePeriodics.Next():
			// instance periodics are rate-limited and may be time-consuming in large
			// clusters, so launch them in the background.
			go a.doInstancePeriodics(ctx)
		case <-ossDesktopsCheck:
			a.syncDesktopsLimitAlert(ctx)
		}
	}
}

func (a *Server) doInstancePeriodics(ctx context.Context) {
	const slowRate = time.Millisecond * 200 // 5 reads per second
	const fastRate = time.Millisecond * 5   // 200 reads per second
	const dynamicPeriod = time.Minute * 3

	instances := a.GetInstances(ctx, types.InstanceFilter{})

	// dynamically scale the rate-limiting we apply to reading instances
	// s.t. we read at a progressively faster rate as we observe larger
	// connected instance counts. this isn't a perfect metric, but it errs
	// on the side of slowness, which is preferable for this kind of periodic.
	instanceRate := slowRate
	if ci := a.inventory.ConnectedInstances(); ci > 0 {
		localDynamicRate := dynamicPeriod / time.Duration(ci)
		if localDynamicRate < fastRate {
			localDynamicRate = fastRate
		}

		if localDynamicRate < instanceRate {
			instanceRate = localDynamicRate
		}
	}

	limiter := rate.NewLimiter(rate.Every(instanceRate), 100)
	instances = stream.RateLimit(instances, func() error {
		return limiter.Wait(ctx)
	})

	// cloud deployments shouldn't include control-plane elements in
	// metrics since information about them is not actionable and may
	// produce misleading/confusing results.
	skipControlPlane := modules.GetModules().Features().Cloud

	// set up aggregators for our periodics
	imp := newInstanceMetricsPeriodic()
	uep := newUpgradeEnrollPeriodic()

	// stream all instances to all aggregators
	for instances.Next() {
		if skipControlPlane {
			for _, service := range instances.Item().GetServices() {
				if service.IsControlPlane() {
					continue
				}
			}
		}

		imp.VisitInstance(instances.Item())
		uep.VisitInstance(instances.Item())
	}

	if err := instances.Done(); err != nil {
		log.Warnf("Failed stream instances for periodics: %v", err)
		return
	}

	// set instance metric values
	totalInstancesMetric.Set(float64(imp.TotalInstances()))
	enrolledInUpgradesMetric.Set(float64(imp.TotalEnrolledInUpgrades()))
	for _, upgrader := range []string{types.UpgraderKindKubeController, types.UpgraderKindSystemdUnit} {
		upgraderCountsMetric.WithLabelValues(upgrader).Set(float64(imp.InstancesWithUpgrader(upgrader)))
	}

	// create/delete upgrade enroll prompt as appropriate
	enrollMsg, shouldPrompt := uep.GenerateEnrollPrompt()
	a.handleUpgradeEnrollPrompt(ctx, enrollMsg, shouldPrompt)
}

const (
	upgradeEnrollAlertID = "auto-upgrade-enroll"
)

func (a *Server) handleUpgradeEnrollPrompt(ctx context.Context, msg string, shouldPrompt bool) {
	const alertTTL = time.Minute * 30

	if !shouldPrompt {
		if err := a.DeleteClusterAlert(ctx, upgradeEnrollAlertID); err != nil && !trace.IsNotFound(err) {
			log.Warnf("Failed to delete %s alert: %v", upgradeEnrollAlertID, err)
		}
		return
	}
	alert, err := types.NewClusterAlert(
		upgradeEnrollAlertID,
		msg,
		// Defaulting to "low" severity level. We may want to make this dynamic
		// in the future depending on the distance from up-to-date.
		types.WithAlertSeverity(types.AlertSeverity_LOW),
		types.WithAlertLabel(types.AlertVerbPermit, fmt.Sprintf("%s:%s", types.KindInstance, types.VerbRead)),
		// hide the normal upgrade alert for users who can see this alert as it is
		// generally more actionable/specific.
		types.WithAlertLabel(types.AlertSupersedes, releaseAlertID),
		types.WithAlertLabel(types.AlertOnLogin, "yes"),
		types.WithAlertExpires(a.clock.Now().Add(alertTTL)),
	)
	if err != nil {
		log.Warnf("Failed to build %s alert: %v (this is a bug)", releaseAlertID, err)
		return
	}
	if err := a.UpsertClusterAlert(ctx, alert); err != nil {
		log.Warnf("Failed to set %s alert: %v", releaseAlertID, err)
		return
	}
}

const (
	releaseAlertID = "upgrade-suggestion"
	secAlertID     = "security-patch-available"
	verInUseLabel  = "teleport.internal/ver-in-use"
)

// syncReleaseAlerts calculates alerts related to new teleport releases. When checkRemote
// is true it pulls the latest release info from github.  Otherwise, it loads the versions used
// for the most recent alerts and re-syncs with latest cluster state.
func (a *Server) syncReleaseAlerts(ctx context.Context, checkRemote bool) {
	log.Debug("Checking for new teleport releases via github api.")

	// NOTE: essentially everything in this function is going to be
	// scrapped/replaced once the inventory and version-control systems
	// are a bit further along.

	current := vc.NewTarget(vc.Normalize(teleport.Version))

	// this environment variable is "unstable" since it will be deprecated
	// by an upcoming tctl command. currently exists for testing purposes only.
	if t := vc.NewTarget(os.Getenv("TELEPORT_UNSTABLE_VC_VERSION")); t.Ok() {
		current = t
	}

	visitor := vc.Visitor{
		Current: current,
	}

	// users cannot upgrade their own auth instances in cloud, so it isn't helpful
	// to generate alerts for releases newer than the current auth server version.
	if modules.GetModules().Features().Cloud {
		visitor.NotNewerThan = current
	}

	var loadFailed bool

	if checkRemote {
		// scrape the github releases API with our visitor
		if err := github.Visit(&visitor); err != nil {
			log.Warnf("Failed to load github releases: %v (this will not impact teleport functionality)", err)
			loadFailed = true
		}
	} else {
		if err := a.visitCachedAlertVersions(ctx, &visitor); err != nil {
			log.Warnf("Failed to load release alert into: %v (this will not impact teleport functionality)", err)
			loadFailed = true
		}
	}

	a.doReleaseAlertSync(ctx, current, visitor, !loadFailed)
}

// visitCachedAlertVersions updates the visitor with targets reconstructed from the metadata
// of existing alerts. This lets us "reevaluate" the alerts based on newer cluster state without
// re-pulling the releases page. Future version of teleport will cache actual full release
// descriptions, rending this unnecessary.
func (a *Server) visitCachedAlertVersions(ctx context.Context, visitor *vc.Visitor) error {
	// reconstruct the target for the "latest stable" alert if it exists.
	alert, err := a.getClusterAlert(ctx, releaseAlertID)
	if err != nil && !trace.IsNotFound(err) {
		return trace.Wrap(err)
	}
	if err == nil {
		if t := vc.NewTarget(alert.Metadata.Labels[verInUseLabel]); t.Ok() {
			visitor.Visit(t)
		}
	}

	// reconstruct the target for the "latest sec patch" alert if it exists.
	alert, err = a.getClusterAlert(ctx, secAlertID)
	if err != nil && !trace.IsNotFound(err) {
		return trace.Wrap(err)
	}
	if err == nil {
		if t := vc.NewTarget(alert.Metadata.Labels[verInUseLabel], vc.SecurityPatch(true)); t.Ok() {
			visitor.Visit(t)
		}
	}
	return nil
}

func (a *Server) getClusterAlert(ctx context.Context, id string) (types.ClusterAlert, error) {
	alerts, err := a.GetClusterAlerts(ctx, types.GetClusterAlertsRequest{
		AlertID: id,
	})
	if err != nil {
		return types.ClusterAlert{}, trace.Wrap(err)
	}
	if len(alerts) == 0 {
		return types.ClusterAlert{}, trace.NotFound("cluster alert %q not found", id)
	}
	return alerts[0], nil
}

func (a *Server) doReleaseAlertSync(ctx context.Context, current vc.Target, visitor vc.Visitor, cleanup bool) {
	const alertTTL = time.Minute * 30
	// use visitor to find the oldest version among connected instances.
	// TODO(fspmarshall): replace this check as soon as we have a backend inventory repr. using
	// connected instances is a poor approximation and may lead to missed notifications if auth
	// server is up to date, but instances not connected to this auth need update.
	var instanceVisitor vc.Visitor
	a.inventory.Iter(func(handle inventory.UpstreamHandle) {
		v := vc.Normalize(handle.Hello().Version)
		instanceVisitor.Visit(vc.NewTarget(v))
	})

	if sp := visitor.NewestSecurityPatch(); sp.Ok() && sp.NewerThan(current) && !sp.SecurityPatchAltOf(current) {
		// explicit security patch alerts have a more limited audience, so we generate
		// them as their own separate alert.
		log.Warnf("A newer security patch has been detected. current=%s, patch=%s", current.Version(), sp.Version())
		secMsg := fmt.Sprintf("A security patch is available for Teleport. Please upgrade your Cluster to %s or newer.", sp.Version())

		alert, err := types.NewClusterAlert(
			secAlertID,
			secMsg,
			types.WithAlertLabel(types.AlertOnLogin, "yes"),
			// TODO(fspmarshall): permit alert to be shown to those with inventory management
			// permissions once we have RBAC around that. For now, token:write is a decent
			// approximation and will ensure that alerts are shown to the editor role.
			types.WithAlertLabel(types.AlertVerbPermit, fmt.Sprintf("%s:%s", types.KindToken, types.VerbCreate)),
			// hide the normal upgrade alert for users who can see this alert in order to
			// improve its visibility and reduce clutter.
			types.WithAlertLabel(types.AlertSupersedes, releaseAlertID),
			types.WithAlertSeverity(types.AlertSeverity_HIGH),
			types.WithAlertLabel(verInUseLabel, sp.Version()),
			types.WithAlertExpires(a.clock.Now().Add(alertTTL)),
		)
		if err != nil {
			log.Warnf("Failed to build %s alert: %v (this is a bug)", secAlertID, err)
			return
		}

		if err := a.UpsertClusterAlert(ctx, alert); err != nil {
			log.Warnf("Failed to set %s alert: %v", secAlertID, err)
			return
		}
	} else if cleanup {
		err := a.DeleteClusterAlert(ctx, secAlertID)
		if err != nil && !trace.IsNotFound(err) {
			log.Warnf("Failed to delete %s alert: %v", secAlertID, err)
		}
	}
}

// updateVersionMetrics leverages the inventory control stream to report the versions of
// all instances that are connected to a single auth server via prometheus metrics. To
// get an accurate representation of versions in an entire cluster the metric must be aggregated
// with all auth instances.
func (a *Server) updateVersionMetrics() {
	versionCount := make(map[string]int)

	// record versions for all connected resources
	a.inventory.Iter(func(handle inventory.UpstreamHandle) {
		versionCount[handle.Hello().Version]++
	})

	// record version for **THIS** auth server
	versionCount[teleport.Version]++

	// reset the gauges so that any versions that fall off are removed from exported metrics
	registeredAgents.Reset()
	for version, count := range versionCount {
		registeredAgents.WithLabelValues(version).Set(float64(count))
	}
}

// updateInstallMethodsMetrics leverages the inventory control stream to report the install methods
// of all instances that are connected to a single auth server via prometheus metrics.
// To get an accurate representation of install methods in an entire cluster the metric must be aggregated
// with all auth instances.
func (a *Server) updateInstallMethodsMetrics() {
	installMethodCount := make(map[string]int)

	// record install methods for all connected resources
	a.inventory.Iter(func(handle inventory.UpstreamHandle) {
		installMethod := "unknown"
		installMethods := append([]string{}, handle.AgentMetadata().InstallMethods...)

		if len(installMethods) > 0 {
			slices.Sort(installMethods)
			installMethod = strings.Join(installMethods, ",")
		}

		installMethodCount[installMethod]++
	})

	// reset the gauges so that any versions that fall off are removed from exported metrics
	registeredAgentsInstallMethod.Reset()
	for installMethod, count := range installMethodCount {
		registeredAgentsInstallMethod.WithLabelValues(installMethod).Set(float64(count))
	}
}

var (
	// remoteClusterRefreshLimit is the maximum number of backend updates that will be performed
	// during periodic remote cluster connection status refresh.
	remoteClusterRefreshLimit = 50

	// remoteClusterRefreshBuckets is the maximum number of refresh cycles that should guarantee the status update
	// of all remote clusters if their number exceeds remoteClusterRefreshLimit × remoteClusterRefreshBuckets.
	remoteClusterRefreshBuckets = 12
)

// refreshRemoteClusters updates connection status of all remote clusters.
func (a *Server) refreshRemoteClusters(ctx context.Context, rnd *insecurerand.Rand) {
	remoteClusters, err := a.Services.GetRemoteClusters()
	if err != nil {
		log.WithError(err).Error("Failed to load remote clusters for status refresh")
		return
	}

	netConfig, err := a.GetClusterNetworkingConfig(ctx)
	if err != nil {
		log.WithError(err).Error("Failed to load networking config for remote cluster status refresh")
		return
	}

	// randomize the order to optimize for multiple auth servers running in parallel
	rnd.Shuffle(len(remoteClusters), func(i, j int) {
		remoteClusters[i], remoteClusters[j] = remoteClusters[j], remoteClusters[i]
	})

	// we want to limit the number of backend updates performed on each refresh to avoid overwhelming the backend.
	updateLimit := remoteClusterRefreshLimit
	if dynamicLimit := (len(remoteClusters) / remoteClusterRefreshBuckets) + 1; dynamicLimit > updateLimit {
		// if the number of remote clusters is larger than remoteClusterRefreshLimit × remoteClusterRefreshBuckets,
		// bump the limit to make sure all remote clusters will be updated within reasonable time.
		updateLimit = dynamicLimit
	}

	var updateCount int
	for _, remoteCluster := range remoteClusters {
		if updated, err := a.updateRemoteClusterStatus(ctx, netConfig, remoteCluster); err != nil {
			log.WithError(err).Error("Failed to perform remote cluster status refresh")
		} else if updated {
			updateCount++
		}

		if updateCount >= updateLimit {
			break
		}
	}
}

func (a *Server) Close() error {
	a.cancelFunc()

	var errs []error

	if err := a.inventory.Close(); err != nil {
		errs = append(errs, err)
	}

	if a.Services.AuditLogSessionStreamer != nil {
		if err := a.Services.AuditLogSessionStreamer.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	if a.bk != nil {
		if err := a.bk.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	return trace.NewAggregate(errs...)
}

func (a *Server) GetClock() clockwork.Clock {
	a.lock.RLock()
	defer a.lock.RUnlock()
	return a.clock
}

// SetClock sets clock, used in tests
func (a *Server) SetClock(clock clockwork.Clock) {
	a.lock.Lock()
	defer a.lock.Unlock()
	a.clock = clock
}

// SetAuditLog sets the server's audit log
func (a *Server) SetAuditLog(auditLog events.AuditLogSessionStreamer) {
	a.Services.AuditLogSessionStreamer = auditLog
}

// GetEmitter fetches the current audit log emitter implementation.
func (a *Server) GetEmitter() apievents.Emitter {
	return a.emitter
}

// SetEmitter sets the current audit log emitter. Note that this is only safe to
// use before main server start.
func (a *Server) SetEmitter(emitter apievents.Emitter) {
	a.emitter = emitter
}

// EmitAuditEvent implements [apievents.Emitter] by delegating to its dedicated
// emitter rather than falling back to the implementation from [Services] (using
// the audit log directly, which is almost never what you want).
func (a *Server) EmitAuditEvent(ctx context.Context, e apievents.AuditEvent) error {
	return trace.Wrap(a.emitter.EmitAuditEvent(ctx, e))
}

// SetUsageReporter sets the server's usage reporter. Note that this is only
// safe to use before server start.
func (a *Server) SetUsageReporter(reporter usagereporter.UsageReporter) {
	a.Services.UsageReporter = reporter
}

// GetDomainName returns the domain name that identifies this authority server.
// Also known as "cluster name"
func (a *Server) GetDomainName() (string, error) {
	clusterName, err := a.GetClusterName()
	if err != nil {
		return "", trace.Wrap(err)
	}
	return clusterName.GetClusterName(), nil
}

// GetClusterCACert returns the PEM-encoded TLS certs for the local cluster. If
// the cluster has multiple TLS certs, they will all be concatenated.
func (a *Server) GetClusterCACert(ctx context.Context) (*proto.GetClusterCACertResponse, error) {
	clusterName, err := a.GetClusterName()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	// Extract the TLS CA for this cluster.
	hostCA, err := a.GetCertAuthority(ctx, types.CertAuthID{
		Type:       types.HostCA,
		DomainName: clusterName.GetClusterName(),
	}, false)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	certs := services.GetTLSCerts(hostCA)
	if len(certs) < 1 {
		return nil, trace.NotFound("no tls certs found in host CA")
	}
	allCerts := bytes.Join(certs, []byte("\n"))

	return &proto.GetClusterCACertResponse{
		TLSCA: allCerts,
	}, nil
}

// GenerateHostCert uses the private key of the CA to sign the public key of the host
// (along with meta data like host ID, node name, roles, and ttl) to generate a host certificate.
func (a *Server) GenerateHostCert(ctx context.Context, hostPublicKey []byte, hostID, nodeName string, principals []string, clusterName string, role types.SystemRole, ttl time.Duration) ([]byte, error) {
	domainName, err := a.GetDomainName()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// get the certificate authority that will be signing the public key of the host
	ca, err := a.Services.GetCertAuthority(ctx, types.CertAuthID{
		Type:       types.HostCA,
		DomainName: domainName,
	}, true)
	if err != nil {
		return nil, trace.BadParameter("failed to load host CA for %q: %v", domainName, err)
	}

	caSigner, err := a.keyStore.GetSSHSigner(ctx, ca)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// create and sign!
	return a.generateHostCert(ctx, services.HostCertParams{
		CASigner:      caSigner,
		PublicHostKey: hostPublicKey,
		HostID:        hostID,
		NodeName:      nodeName,
		Principals:    principals,
		ClusterName:   clusterName,
		Role:          role,
		TTL:           ttl,
	})
}

func (a *Server) generateHostCert(
	ctx context.Context, p services.HostCertParams,
) ([]byte, error) {
	authPref, err := a.GetAuthPreference(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	var locks []types.LockTarget
	switch p.Role {
	case types.RoleNode:
		// Node role is a special case because it was previously suported as a
		// lock target that only locked the `ssh_service`. If the same Teleport server
		// had multiple roles, Node lock would only lock the `ssh_service` while
		// other roles would be able to generate certificates without a problem.
		// To remove the ambiguity, we now lock the entire Teleport server for
		// all roles using the LockTarget.ServerID field and `Node` field is
		// deprecated.
		// In order to support legacy behavior, we need fill in both `ServerID`
		// and `Node` fields if the role is `Node` so that the previous behavior
		// is preserved.
		// This is a legacy behavior that we need to support for backwards compatibility.
		locks = []types.LockTarget{{ServerID: p.HostID, Node: p.HostID}, {ServerID: HostFQDN(p.HostID, p.ClusterName), Node: HostFQDN(p.HostID, p.ClusterName)}}
	default:
		locks = []types.LockTarget{{ServerID: p.HostID}, {ServerID: HostFQDN(p.HostID, p.ClusterName)}}
	}
	if lockErr := a.checkLockInForce(authPref.GetLockingMode(),
		locks,
	); lockErr != nil {
		return nil, trace.Wrap(lockErr)
	}

	return a.Authority.GenerateHostCert(p)
}

// GetKeyStore returns the KeyStore used by the auth server
func (a *Server) GetKeyStore() *keystore.Manager {
	return a.keyStore
}

type certRequest struct {
	// user is a user to generate certificate for
	user services.UserState
	// impersonator is a user who generates the certificate,
	// is set when different from the user in the certificate
	impersonator string
	// checker is used to perform RBAC checks.
	checker services.AccessChecker
	// ttl is Duration of the certificate
	ttl time.Duration
	// publicKey is RSA public key in authorized_keys format
	publicKey []byte
	// compatibility is compatibility mode
	compatibility string
	// overrideRoleTTL is used for requests when the requested TTL should not be
	// adjusted based off the role of the user. This is used by tctl to allow
	// creating long lived user certs.
	overrideRoleTTL bool
	// usage is a list of acceptable usages to be encoded in X509 certificate,
	// is used to limit ways the certificate can be used, for example
	// the cert can be only used against kubernetes endpoint, and not auth endpoint,
	// no usage means unrestricted (to keep backwards compatibility)
	usage []string
	// routeToCluster is an optional teleport cluster name to route the
	// certificate requests to, this teleport cluster name will be used to
	// route the requests to in case of kubernetes
	routeToCluster string
	// kubernetesCluster specifies the target kubernetes cluster for TLS
	// identities. This can be empty on older Teleport clients.
	kubernetesCluster string
	// traits hold claim data used to populate a role at runtime.
	traits wrappers.Traits
	// activeRequests tracks privilege escalation requests applied
	// during the construction of the certificate.
	activeRequests services.RequestIDs
	// appSessionID is the session ID of the application session.
	appSessionID string
	// appPublicAddr is the public address of the application.
	appPublicAddr string
	// appClusterName is the name of the cluster this application is in.
	appClusterName string
	// appName is the name of the application to generate cert for.
	appName string
	// awsRoleARN is the role ARN to generate certificate for.
	awsRoleARN string
	// azureIdentity is the Azure identity to generate certificate for.
	azureIdentity string
	// gcpServiceAccount is the GCP service account to generate certificate for.
	gcpServiceAccount string
	// dbService identifies the name of the database service requests will
	// be routed to.
	dbService string
	// dbProtocol specifies the protocol of the database a certificate will
	// be issued for.
	dbProtocol string
	// dbUser is the optional database user which, if provided, will be used
	// as a default username.
	dbUser string
	// dbName is the optional database name which, if provided, will be used
	// as a default database.
	dbName string
	// mfaVerified is the UUID of an MFA device when this certRequest was
	// created immediately after an MFA check.
	mfaVerified string
	// previousIdentityExpires is the expiry time of the identity/cert that this
	// identity/cert was derived from. It is used to determine a session's hard
	// deadline in cases where both require_session_mfa and disconnect_expired_cert
	// are enabled. See https://github.com/gravitational/teleport/issues/18544.
	previousIdentityExpires time.Time
	// loginIP is an IP of the client requesting the certificate.
	loginIP string
	// pinIP flags that client's login IP should be pinned in the certificate
	pinIP bool
	// disallowReissue flags that a cert should not be allowed to issue future
	// certificates.
	disallowReissue bool
	// renewable indicates that the certificate can be renewed,
	// having its TTL increased
	renewable bool
	// includeHostCA indicates that host CA certs should be included in the
	// returned certs
	includeHostCA bool
	// generation indicates the number of times this certificate has been
	// renewed.
	generation uint64
	// connectionDiagnosticID contains the ID of the ConnectionDiagnostic.
	// The Node/Agent will append connection traces to this instance.
	connectionDiagnosticID string
	// attestationStatement is an attestation statement associated with the given public key.
	attestationStatement *keys.AttestationStatement
	// skipAttestation is a server-side flag which is used to skip the attestation check.
	skipAttestation bool
	// deviceExtensions holds device-aware user certificate extensions.
	deviceExtensions DeviceExtensions
}

// check verifies the cert request is valid.
func (r *certRequest) check() error {
	if r.user == nil {
		return trace.BadParameter("missing parameter user")
	}
	if r.checker == nil {
		return trace.BadParameter("missing parameter checker")
	}

	// When generating certificate for MongoDB access, database username must
	// be encoded into it. This is required to be able to tell which database
	// user to authenticate the connection as.
	if r.dbProtocol == defaults.ProtocolMongoDB {
		if r.dbUser == "" {
			return trace.BadParameter("must provide database user name to generate certificate for database %q", r.dbService)
		}
	}
	return nil
}

type certRequestOption func(*certRequest)

func certRequestMFAVerified(mfaID string) certRequestOption {
	return func(r *certRequest) { r.mfaVerified = mfaID }
}

func certRequestPreviousIdentityExpires(previousIdentityExpires time.Time) certRequestOption {
	return func(r *certRequest) { r.previousIdentityExpires = previousIdentityExpires }
}

func certRequestLoginIP(ip string) certRequestOption {
	return func(r *certRequest) { r.loginIP = ip }
}

func certRequestDeviceExtensions(ext tlsca.DeviceExtensions) certRequestOption {
	return func(r *certRequest) {
		r.deviceExtensions = DeviceExtensions(ext)
	}
}

// getUserOrLoginState will return the given user or the login state associated with the user.
func (a *Server) getUserOrLoginState(ctx context.Context, username string) (services.UserState, error) {
	uls, err := a.GetUserLoginState(ctx, username)
	if err != nil && !trace.IsNotFound(err) {
		return nil, trace.Wrap(err)
	}

	if err == nil {
		return uls, nil
	}

	user, err := a.GetUser(username, false)
	return user, trace.Wrap(err)
}

func (a *Server) GenerateOpenSSHCert(ctx context.Context, req *proto.OpenSSHCertRequest) (*proto.OpenSSHCert, error) {
	if req.User == nil {
		return nil, trace.BadParameter("user is empty")
	}
	if len(req.PublicKey) == 0 {
		return nil, trace.BadParameter("public key is empty")
	}
	if req.TTL == 0 {
		cap, err := a.GetAuthPreference(ctx)
		if err != nil {
			return nil, trace.BadParameter("cert request does not specify a TTL and the cluster_auth_preference is not available: %v", err)
		}
		req.TTL = proto.Duration(cap.GetDefaultSessionTTL())
	}
	if req.TTL < 0 {
		return nil, trace.BadParameter("TTL must be positive")
	}
	if req.Cluster == "" {
		return nil, trace.BadParameter("cluster is empty")
	}

	// add implicit roles to the set and build a checker
	accessInfo := services.AccessInfoFromUserState(req.User)
	roles := make([]types.Role, len(req.Roles))
	for i := range req.Roles {
		var err error
		roles[i], err = services.ApplyTraits(req.Roles[i], req.User.GetTraits())
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}
	roleSet := services.NewRoleSet(roles...)

	clusterName, err := a.GetClusterName()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	checker := services.NewAccessCheckerWithRoleSet(accessInfo, clusterName.GetClusterName(), roleSet)

	certs, err := a.generateOpenSSHCert(certRequest{
		user:            req.User,
		publicKey:       req.PublicKey,
		compatibility:   constants.CertificateFormatStandard,
		checker:         checker,
		ttl:             time.Duration(req.TTL),
		traits:          req.User.GetTraits(),
		routeToCluster:  req.Cluster,
		disallowReissue: true,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return &proto.OpenSSHCert{
		Cert: certs.SSH,
	}, nil
}

// GenerateUserTestCertsRequest is a request to generate test certificates.
type GenerateUserTestCertsRequest struct {
	Key            []byte
	Username       string
	TTL            time.Duration
	Compatibility  string
	RouteToCluster string
	PinnedIP       string
	MFAVerified    string
}

// GenerateUserTestCerts is used to generate user certificate, used internally for tests
func (a *Server) GenerateUserTestCerts(req GenerateUserTestCertsRequest) ([]byte, []byte, error) {
	userState, err := a.getUserOrLoginState(context.Background(), req.Username)
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}
	accessInfo := services.AccessInfoFromUserState(userState)
	clusterName, err := a.GetClusterName()
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}
	checker, err := services.NewAccessChecker(accessInfo, clusterName.GetClusterName(), a)
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}
	certs, err := a.generateUserCert(certRequest{
		user:           userState,
		ttl:            req.TTL,
		compatibility:  req.Compatibility,
		publicKey:      req.Key,
		routeToCluster: req.RouteToCluster,
		checker:        checker,
		traits:         userState.GetTraits(),
		loginIP:        req.PinnedIP,
		pinIP:          req.PinnedIP != "",
		mfaVerified:    req.MFAVerified,
	})
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}
	return certs.SSH, certs.TLS, nil
}

// AppTestCertRequest combines parameters for generating a test app access cert.
type AppTestCertRequest struct {
	// PublicKey is the public key to sign.
	PublicKey []byte
	// Username is the Teleport user name to sign certificate for.
	Username string
	// TTL is the test certificate validity period.
	TTL time.Duration
	// PublicAddr is the application public address. Used for routing.
	PublicAddr string
	// ClusterName is the name of the cluster application resides in. Used for routing.
	ClusterName string
	// SessionID is the optional session ID to encode. Used for routing.
	SessionID string
	// AWSRoleARN is optional AWS role ARN a user wants to assume to encode.
	AWSRoleARN string
	// AzureIdentity is the optional Azure identity a user wants to assume to encode.
	AzureIdentity string
	// GCPServiceAccount is optional GCP service account a user wants to assume to encode.
	GCPServiceAccount string
	// PinnedIP is optional IP to pin certificate to.
	PinnedIP string
	// LoginTrait is the login to include in the cert
	LoginTrait string
}

// GenerateUserAppTestCert generates an application specific certificate, used
// internally for tests.
func (a *Server) GenerateUserAppTestCert(req AppTestCertRequest) ([]byte, error) {
	userState, err := a.getUserOrLoginState(context.Background(), req.Username)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	accessInfo := services.AccessInfoFromUserState(userState)
	clusterName, err := a.GetClusterName()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	checker, err := services.NewAccessChecker(accessInfo, clusterName.GetClusterName(), a)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = uuid.New().String()
	}

	login := req.LoginTrait
	if login == "" {
		login = uuid.New().String()
	}

	certs, err := a.generateUserCert(certRequest{
		user:      userState,
		publicKey: req.PublicKey,
		checker:   checker,
		ttl:       req.TTL,
		// Set the login to be a random string. Application certificates are never
		// used to log into servers but SSH certificate generation code requires a
		// principal be in the certificate.
		traits: wrappers.Traits(map[string][]string{
			constants.TraitLogins: {login},
		}),
		// Only allow this certificate to be used for applications.
		usage: []string{teleport.UsageAppsOnly},
		// Add in the application routing information.
		appSessionID:      sessionID,
		appPublicAddr:     req.PublicAddr,
		appClusterName:    req.ClusterName,
		awsRoleARN:        req.AWSRoleARN,
		azureIdentity:     req.AzureIdentity,
		gcpServiceAccount: req.GCPServiceAccount,
		pinIP:             req.PinnedIP != "",
		loginIP:           req.PinnedIP,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return certs.TLS, nil
}

// DatabaseTestCertRequest combines parameters for generating test database
// access certificate.
type DatabaseTestCertRequest struct {
	// PublicKey is the public key to sign.
	PublicKey []byte
	// Cluster is the Teleport cluster name.
	Cluster string
	// Username is the Teleport username.
	Username string
	// RouteToDatabase contains database routing information.
	RouteToDatabase tlsca.RouteToDatabase
	// PinnedIP is an IP new certificate should be pinned to.
	PinnedIP string
}

// GenerateDatabaseTestCert generates a database access certificate for the
// provided parameters. Used only internally in tests.
func (a *Server) GenerateDatabaseTestCert(req DatabaseTestCertRequest) ([]byte, error) {
	userState, err := a.getUserOrLoginState(context.Background(), req.Username)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	accessInfo := services.AccessInfoFromUserState(userState)
	clusterName, err := a.GetClusterName()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	checker, err := services.NewAccessChecker(accessInfo, clusterName.GetClusterName(), a)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	certs, err := a.generateUserCert(certRequest{
		user:      userState,
		publicKey: req.PublicKey,
		loginIP:   req.PinnedIP,
		pinIP:     req.PinnedIP != "",
		checker:   checker,
		ttl:       time.Hour,
		traits: map[string][]string{
			constants.TraitLogins: {req.Username},
		},
		routeToCluster: req.Cluster,
		dbService:      req.RouteToDatabase.ServiceName,
		dbProtocol:     req.RouteToDatabase.Protocol,
		dbUser:         req.RouteToDatabase.Username,
		dbName:         req.RouteToDatabase.Database,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return certs.TLS, nil
}

// DeviceExtensions hold device-aware user certificate extensions.
// Device extensions are a part of Device Trust, a feature exclusive to Teleport
// Enterprise.
type DeviceExtensions tlsca.DeviceExtensions

// AugmentUserCertificateOpts aggregates options for extending user
// certificates.
// See [AugmentContextUserCertificates].
type AugmentUserCertificateOpts struct {
	// SSHAuthorizedKey is an SSH certificate, in the authorized key format, to
	// augment with opts.
	// The SSH certificate must be issued for the current authenticated user and
	// must match their TLS certificate.
	SSHAuthorizedKey []byte
	// DeviceExtensions are the device-aware extensions to add to the certificates
	// being augmented.
	DeviceExtensions *DeviceExtensions
}

// AugmentContextUserCertificates augments the context user certificates with
// the given extensions. It requires the user's TLS certificate to be present
// in the [ctx], in addition to the [authCtx] itself.
//
// Any additional certificates to augment, such as the SSH certificate, must be
// valid and fully match the certificate used to authenticate (likely the user's
// mTLS cert).
//
// Used by Device Trust to add device extensions to the user certificate.
func (a *Server) AugmentContextUserCertificates(
	ctx context.Context,
	authCtx *authz.Context, opts *AugmentUserCertificateOpts,
) (*proto.Certs, error) {
	switch {
	case authCtx == nil:
		return nil, trace.BadParameter("authCtx required")
	case opts == nil:
		return nil, trace.BadParameter("opts required")
	}

	// Is at least one extension present?
	// Are the extensions valid?
	identity := authCtx.Identity.GetIdentity()
	dev := opts.DeviceExtensions
	switch {
	case dev == nil: // Only extension that currently exists.
		return nil, trace.BadParameter("at least one opts extension must be present")
	case dev.DeviceID == "":
		return nil, trace.BadParameter("opts.DeviceExtensions.DeviceID required")
	case dev.AssetTag == "":
		return nil, trace.BadParameter("opts.DeviceExtensions.AssetTag required")
	case dev.CredentialID == "":
		return nil, trace.BadParameter("opts.DeviceExtensions.CredentialID required")
	// Do not reissue if device extensions are already present.
	case identity.DeviceExtensions.DeviceID != "",
		identity.DeviceExtensions.AssetTag != "",
		identity.DeviceExtensions.CredentialID != "":
		return nil, trace.BadParameter("device extensions already present")
	}

	// Fetch user TLS certificate.
	x509Cert, err := authz.UserCertificateFromContext(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Sanity check: x509Cert matches identity.
	// Both the TLS certificate and the identity come from the same source, so
	// they are unlikely to mismatch unless Teleport itself mixes it up.
	if x509Cert.Subject.CommonName != identity.Username {
		return nil, trace.BadParameter("identity and x509 user mismatch")
	}

	// Parse and verify SSH certificate.
	sshAuthorizedKey := opts.SSHAuthorizedKey
	var sshCert *ssh.Certificate
	if len(sshAuthorizedKey) > 0 {
		var err error
		sshCert, err = apisshutils.ParseCertificate(sshAuthorizedKey)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		xPubKey, err := ssh.NewPublicKey(x509Cert.PublicKey)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		// filter and sort TLS and SSH principals for comparison.
		// Order does not matter and "-teleport-*" principals are filtered out.
		filterAndSortPrincipals := func(s []string) []string {
			res := make([]string, 0, len(s))
			for _, principal := range s {
				// Ignore -teleport- internal principals.
				if strings.HasPrefix(principal, "-teleport-") {
					continue
				}
				res = append(res, principal)
			}
			sort.Strings(res)
			return res
		}

		// Verify SSH certificate against identity.
		// The SSH certificate isn't used to establish the connection that
		// eventually reaches this method, so we check it more thoroughly.
		// In the end it still has to be signed by the Teleport CA and share the
		// TLS public key, but we verify most fields to be safe.
		switch {
		case sshCert.CertType != ssh.UserCert:
			return nil, trace.BadParameter("ssh cert type mismatch")
		case sshCert.KeyId != identity.Username:
			return nil, trace.BadParameter("identity and SSH user mismatch")
		case !slices.Equal(filterAndSortPrincipals(sshCert.ValidPrincipals), filterAndSortPrincipals(identity.Principals)):
			return nil, trace.BadParameter("identity and SSH principals mismatch")
		case !apisshutils.KeysEqual(sshCert.Key, xPubKey):
			return nil, trace.BadParameter("x509 and SSH public key mismatch")
		// Do not reissue if device extensions are already present.
		case sshCert.Extensions[teleport.CertExtensionDeviceID] != "",
			sshCert.Extensions[teleport.CertExtensionDeviceAssetTag] != "",
			sshCert.Extensions[teleport.CertExtensionDeviceCredentialID] != "":
			return nil, trace.BadParameter("device extensions already present")
		}
	}

	// Fetch TLS CA and SSH signer.
	domainName, err := a.GetDomainName()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	tlsCA, sshSigner, _, err := a.getSigningCAs(ctx, domainName, types.UserCA)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Verify TLS certificate against CA.
	now := a.clock.Now()
	roots := x509.NewCertPool()
	roots.AddCert(tlsCA.Cert)
	if _, err := x509Cert.Verify(x509.VerifyOptions{
		Roots:       roots,
		CurrentTime: now,
		KeyUsages: []x509.ExtKeyUsage{
			// Extensions added by tlsca.
			// See https://github.com/gravitational/teleport/blob/master/lib/tlsca/ca.go#L963.
			x509.ExtKeyUsageServerAuth,
			x509.ExtKeyUsageClientAuth,
		},
	}); err != nil {
		return nil, trace.Wrap(err)
	}

	// Verify SSH certificate against CA.
	if sshCert != nil {
		// ValidPrincipals are checked against identity above.
		// Pick the first one from the cert here.
		var principal string
		if len(sshCert.ValidPrincipals) > 0 {
			principal = sshCert.ValidPrincipals[0]
		}

		certChecker := &ssh.CertChecker{
			Clock: a.clock.Now,
		}
		if err := certChecker.CheckCert(principal, sshCert); err != nil {
			return nil, trace.Wrap(err)
		}

		// CheckCert verifies the signature but not the CA.
		// Do that here.
		if !apisshutils.KeysEqual(sshCert.SignatureKey, sshSigner.PublicKey()) {
			return nil, trace.BadParameter("ssh certificate signed by unknown authority")
		}
	}

	// Verify locks right before we re-issue any certificates.
	authPref, err := a.GetAuthPreference(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if err := a.verifyLocksForUserCerts(verifyLocksForUserCertsReq{
		checker:              authCtx.Checker,
		defaultMode:          authPref.GetLockingMode(),
		username:             identity.Username,
		mfaVerified:          identity.MFAVerified,
		activeAccessRequests: identity.ActiveRequests,
		deviceID:             opts.DeviceExtensions.DeviceID, // Check lock against requested device.
	}); err != nil {
		return nil, trace.Wrap(err)
	}

	// Augment TLS certificate.
	newIdentity := identity
	newIdentity.DeviceExtensions.DeviceID = dev.DeviceID
	newIdentity.DeviceExtensions.AssetTag = dev.AssetTag
	newIdentity.DeviceExtensions.CredentialID = dev.CredentialID
	subj, err := newIdentity.Subject()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	newTLSCert, err := tlsCA.GenerateCertificate(tlsca.CertificateRequest{
		Clock:     a.clock,
		PublicKey: x509Cert.PublicKey,
		Subject:   subj,
		// Use the same expiration as the original cert.
		NotAfter: x509Cert.NotAfter,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Augment SSH certificate.
	var newAuthorizedKey []byte
	if sshCert != nil {
		// Add some leeway to validAfter to avoid time skew errors.
		validAfter := a.clock.Now().UTC().Add(-1 * time.Minute)
		newSSHCert := &ssh.Certificate{
			Key:             sshCert.Key,
			CertType:        ssh.UserCert,
			KeyId:           sshCert.KeyId,
			ValidPrincipals: sshCert.ValidPrincipals,
			ValidAfter:      uint64(validAfter.Unix()),
			// Use the same expiration as the x509 cert.
			ValidBefore: uint64(x509Cert.NotAfter.Unix()),
			Permissions: sshCert.Permissions,
		}
		newSSHCert.Extensions[teleport.CertExtensionDeviceID] = dev.DeviceID
		newSSHCert.Extensions[teleport.CertExtensionDeviceAssetTag] = dev.AssetTag
		newSSHCert.Extensions[teleport.CertExtensionDeviceCredentialID] = dev.CredentialID
		if err := newSSHCert.SignCert(rand.Reader, sshSigner); err != nil {
			return nil, trace.Wrap(err)
		}
		newAuthorizedKey = ssh.MarshalAuthorizedKey(newSSHCert)
	}

	return &proto.Certs{
		SSH: newAuthorizedKey,
		TLS: newTLSCert,
	}, nil
}

// submitCertificateIssuedEvent submits a certificate issued usage event to the
// usage reporting service.
func (a *Server) submitCertificateIssuedEvent(req *certRequest) {
	var database, app, kubernetes, desktop bool

	if req.dbService != "" {
		database = true
	}

	if req.appName != "" {
		app = true
	}

	if req.kubernetesCluster != "" {
		kubernetes = true
	}

	// Bot users are regular Teleport users, but have a special internal label.
	bot := req.user.IsBot()

	// Unfortunately the only clue we have about Windows certs is the usage
	// restriction: `RouteToWindowsDesktop` isn't actually passed along to the
	// certRequest.
	for _, usage := range req.usage {
		switch usage {
		case teleport.UsageWindowsDesktopOnly:
			desktop = true
		}
	}

	// For usage reporting, we care about the impersonator rather than the user
	// being impersonated (if any).
	user := req.user.GetName()
	if req.impersonator != "" {
		user = req.impersonator
	}

	a.AnonymizeAndSubmit(&usagereporter.UserCertificateIssuedEvent{
		UserName:        user,
		Ttl:             durationpb.New(req.ttl),
		IsBot:           bot,
		UsageDatabase:   database,
		UsageApp:        app,
		UsageKubernetes: kubernetes,
		UsageDesktop:    desktop,
	})
}

// generateUserCert generates certificates signed with User CA
func (a *Server) generateUserCert(req certRequest) (*proto.Certs, error) {
	return generateCert(a, req, types.UserCA)
}

// generateOpenSSHCert generates certificates signed with OpenSSH CA
func (a *Server) generateOpenSSHCert(req certRequest) (*proto.Certs, error) {
	return generateCert(a, req, types.OpenSSHCA)
}

func generateCert(a *Server, req certRequest, caType types.CertAuthType) (*proto.Certs, error) {
	ctx := context.TODO()
	err := req.check()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if len(req.checker.GetAllowedResourceIDs()) > 0 && modules.GetModules().BuildType() != modules.BuildEnterprise {
		return nil, fmt.Errorf("Resource Access Requests: %w", ErrRequiresEnterprise)
	}

	// Reject the cert request if there is a matching lock in force.
	authPref, err := a.GetAuthPreference(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if err := a.verifyLocksForUserCerts(verifyLocksForUserCertsReq{
		checker:              req.checker,
		defaultMode:          authPref.GetLockingMode(),
		username:             req.user.GetName(),
		mfaVerified:          req.mfaVerified,
		activeAccessRequests: req.activeRequests.AccessRequests,
		deviceID:             req.deviceExtensions.DeviceID,
	}); err != nil {
		return nil, trace.Wrap(err)
	}

	// reuse the same RSA keys for SSH and TLS keys
	cryptoPubKey, err := sshutils.CryptoPublicKey(req.publicKey)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// extract the passed in certificate format. if nothing was passed in, fetch
	// the certificate format from the role.
	certificateFormat, err := utils.CheckCertificateFormatFlag(req.compatibility)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if certificateFormat == teleport.CertificateFormatUnspecified {
		certificateFormat = req.checker.CertificateFormat()
	}

	var sessionTTL time.Duration
	var allowedLogins []string

	if req.ttl == 0 {
		req.ttl = time.Duration(authPref.GetDefaultSessionTTL())
	}

	// If the role TTL is ignored, do not restrict session TTL and allowed logins.
	// The only caller setting this parameter should be "tctl auth sign".
	// Otherwise, set the session TTL to the smallest of all roles and
	// then only grant access to allowed logins based on that.
	if req.overrideRoleTTL {
		// Take whatever was passed in. Pass in 0 to CheckLoginDuration so all
		// logins are returned for the role set.
		sessionTTL = req.ttl
		allowedLogins, err = req.checker.CheckLoginDuration(0)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	} else {
		// Adjust session TTL to the smaller of two values: the session TTL requested
		// in tsh (possibly using default_session_ttl) or the session TTL for the
		// role.
		sessionTTL = req.checker.AdjustSessionTTL(req.ttl)
		// Return a list of logins that meet the session TTL limit. This means if
		// the requested session TTL is larger than the max session TTL for a login,
		// that login will not be included in the list of allowed logins.
		allowedLogins, err = req.checker.CheckLoginDuration(sessionTTL)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}

	attestedKeyPolicy := keys.PrivateKeyPolicyNone
	requiredKeyPolicy := req.checker.PrivateKeyPolicy(authPref.GetPrivateKeyPolicy())
	if !req.skipAttestation && requiredKeyPolicy != keys.PrivateKeyPolicyNone {
		// verify that the required private key policy for the requesting identity
		// is met by the provided attestation statement.
		attestedKeyPolicy, err = modules.GetModules().AttestHardwareKey(ctx, a, requiredKeyPolicy, req.attestationStatement, cryptoPubKey, sessionTTL)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}

	clusterName, err := a.GetDomainName()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if req.routeToCluster == "" {
		req.routeToCluster = clusterName
	}
	if req.routeToCluster != clusterName {
		// Authorize access to a remote cluster.
		rc, err := a.GetRemoteCluster(req.routeToCluster)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		if err := req.checker.CheckAccessToRemoteCluster(rc); err != nil {
			if trace.IsAccessDenied(err) {
				return nil, trace.NotFound("remote cluster %q not found", req.routeToCluster)
			}
			return nil, trace.Wrap(err)
		}
	}

	// Add the special join-only principal used for joining sessions.
	// All users have access to this and join RBAC rules are checked after the connection is established.
	allowedLogins = append(allowedLogins, teleport.SSHSessionJoinPrincipal)

	requestedResourcesStr, err := types.ResourceIDsToString(req.checker.GetAllowedResourceIDs())
	if err != nil {
		return nil, trace.Wrap(err)
	}

	pinnedIP := ""
	if caType == types.UserCA && (req.checker.PinSourceIP() || req.pinIP) {
		if req.loginIP == "" {
			return nil, trace.BadParameter("IP pinning is enabled for user %q but there is no client IP information", req.user.GetName())
		}

		pinnedIP = req.loginIP
	}

	ca, err := a.GetCertAuthority(ctx, types.CertAuthID{
		Type:       caType,
		DomainName: clusterName,
	}, true)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	sshSigner, err := a.keyStore.GetSSHSigner(ctx, ca)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	params := services.UserCertParams{
		CASigner:                sshSigner,
		PublicUserKey:           req.publicKey,
		Username:                req.user.GetName(),
		Impersonator:            req.impersonator,
		AllowedLogins:           allowedLogins,
		TTL:                     sessionTTL,
		Roles:                   req.checker.RoleNames(),
		CertificateFormat:       certificateFormat,
		PermitPortForwarding:    req.checker.CanPortForward(),
		PermitAgentForwarding:   req.checker.CanForwardAgents(),
		PermitX11Forwarding:     req.checker.PermitX11Forwarding(),
		RouteToCluster:          req.routeToCluster,
		Traits:                  req.traits,
		ActiveRequests:          req.activeRequests,
		MFAVerified:             req.mfaVerified,
		PreviousIdentityExpires: req.previousIdentityExpires,
		LoginIP:                 req.loginIP,
		PinnedIP:                pinnedIP,
		DisallowReissue:         req.disallowReissue,
		Renewable:               req.renewable,
		Generation:              req.generation,
		CertificateExtensions:   req.checker.CertificateExtensions(),
		AllowedResourceIDs:      requestedResourcesStr,
		ConnectionDiagnosticID:  req.connectionDiagnosticID,
		PrivateKeyPolicy:        attestedKeyPolicy,
		DeviceID:                req.deviceExtensions.DeviceID,
		DeviceAssetTag:          req.deviceExtensions.AssetTag,
		DeviceCredentialID:      req.deviceExtensions.CredentialID,
	}
	signedSSHCert, err := a.GenerateUserCert(params)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	kubeGroups, kubeUsers, err := req.checker.CheckKubeGroupsAndUsers(sessionTTL, req.overrideRoleTTL)
	// NotFound errors are acceptable - this user may have no k8s access
	// granted and that shouldn't prevent us from issuing a TLS cert.
	if err != nil && !trace.IsNotFound(err) {
		return nil, trace.Wrap(err)
	}
	// Only validate/default kubernetes cluster name for the current teleport
	// cluster. If this cert is targeting a trusted teleport cluster, leave all
	// the kubernetes cluster validation up to them.
	if req.routeToCluster == clusterName {
		req.kubernetesCluster, err = kubeutils.CheckOrSetKubeCluster(a.closeCtx, a, req.kubernetesCluster, clusterName)
		if err != nil {
			if !trace.IsNotFound(err) {
				return nil, trace.Wrap(err)
			}
			log.Debug("Failed setting default kubernetes cluster for user login (user did not provide a cluster); leaving KubernetesCluster extension in the TLS certificate empty")
		}
	}

	// See which database names and users this user is allowed to use.
	dbNames, dbUsers, err := req.checker.CheckDatabaseNamesAndUsers(sessionTTL, req.overrideRoleTTL)
	if err != nil && !trace.IsNotFound(err) {
		return nil, trace.Wrap(err)
	}

	// See which AWS role ARNs this user is allowed to assume.
	roleARNs, err := req.checker.CheckAWSRoleARNs(sessionTTL, req.overrideRoleTTL)
	if err != nil && !trace.IsNotFound(err) {
		return nil, trace.Wrap(err)
	}

	// See which Azure identities this user is allowed to assume.
	azureIdentities, err := req.checker.CheckAzureIdentities(sessionTTL, req.overrideRoleTTL)
	if err != nil && !trace.IsNotFound(err) {
		return nil, trace.Wrap(err)
	}

	// Enumerate allowed GCP service accounts.
	gcpAccounts, err := req.checker.CheckGCPServiceAccounts(sessionTTL, req.overrideRoleTTL)
	if err != nil && !trace.IsNotFound(err) {
		return nil, trace.Wrap(err)
	}

	identity := tlsca.Identity{
		Username:          req.user.GetName(),
		Impersonator:      req.impersonator,
		Groups:            req.checker.RoleNames(),
		Principals:        allowedLogins,
		Usage:             req.usage,
		RouteToCluster:    req.routeToCluster,
		KubernetesCluster: req.kubernetesCluster,
		Traits:            req.traits,
		KubernetesGroups:  kubeGroups,
		KubernetesUsers:   kubeUsers,
		RouteToApp: tlsca.RouteToApp{
			SessionID:         req.appSessionID,
			PublicAddr:        req.appPublicAddr,
			ClusterName:       req.appClusterName,
			Name:              req.appName,
			AWSRoleARN:        req.awsRoleARN,
			AzureIdentity:     req.azureIdentity,
			GCPServiceAccount: req.gcpServiceAccount,
		},
		TeleportCluster: clusterName,
		RouteToDatabase: tlsca.RouteToDatabase{
			ServiceName: req.dbService,
			Protocol:    req.dbProtocol,
			Username:    req.dbUser,
			Database:    req.dbName,
		},
		DatabaseNames:           dbNames,
		DatabaseUsers:           dbUsers,
		MFAVerified:             req.mfaVerified,
		PreviousIdentityExpires: req.previousIdentityExpires,
		LoginIP:                 req.loginIP,
		PinnedIP:                pinnedIP,
		AWSRoleARNs:             roleARNs,
		AzureIdentities:         azureIdentities,
		GCPServiceAccounts:      gcpAccounts,
		ActiveRequests:          req.activeRequests.AccessRequests,
		DisallowReissue:         req.disallowReissue,
		Renewable:               req.renewable,
		Generation:              req.generation,
		AllowedResourceIDs:      req.checker.GetAllowedResourceIDs(),
		PrivateKeyPolicy:        attestedKeyPolicy,
		ConnectionDiagnosticID:  req.connectionDiagnosticID,
		DeviceExtensions: tlsca.DeviceExtensions{
			DeviceID:     req.deviceExtensions.DeviceID,
			AssetTag:     req.deviceExtensions.AssetTag,
			CredentialID: req.deviceExtensions.CredentialID,
		},
		UserType: req.user.GetUserType(),
	}

	var signedTLSCert []byte
	notAfter := a.clock.Now().UTC().Add(sessionTTL)
	// generate TLS certificate if the signing CA isn't OpenSSH CA, as
	// OpenSSH CA doesn't have any TLS keypairs
	if caType != types.OpenSSHCA {
		tlsCert, tlsSigner, err := a.keyStore.GetTLSCertAndSigner(ctx, ca)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		tlsCA, err := tlsca.FromCertAndSigner(tlsCert, tlsSigner)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		subject, err := identity.Subject()
		if err != nil {
			return nil, trace.Wrap(err)
		}
		certRequest := tlsca.CertificateRequest{
			Clock:     a.clock,
			PublicKey: cryptoPubKey,
			Subject:   subject,
			NotAfter:  notAfter,
		}
		signedTLSCert, err = tlsCA.GenerateCertificate(certRequest)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}

	eventIdentity := identity.GetEventIdentity()
	eventIdentity.Expires = notAfter
	if err := a.emitter.EmitAuditEvent(a.closeCtx, &apievents.CertificateCreate{
		Metadata: apievents.Metadata{
			Type: events.CertificateCreateEvent,
			Code: events.CertificateCreateCode,
		},
		CertificateType: events.CertificateTypeUser,
		Identity:        &eventIdentity,
	}); err != nil {
		log.WithError(err).Warn("Failed to emit certificate create event.")
	}

	// create certs struct to return to user
	certs := &proto.Certs{
		SSH: signedSSHCert,
		TLS: signedTLSCert,
	}

	// always include specified CA
	cas := []types.CertAuthority{ca}

	// also include host CA certs if requested
	if req.includeHostCA {
		hostCA, err := a.GetCertAuthority(ctx, types.CertAuthID{
			Type:       types.HostCA,
			DomainName: clusterName,
		}, false)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		cas = append(cas, hostCA)
	}

	for _, ca := range cas {
		certs.TLSCACerts = append(certs.TLSCACerts, services.GetTLSCerts(ca)...)
		certs.SSHCACerts = append(certs.SSHCACerts, services.GetSSHCheckingKeys(ca)...)
	}

	a.submitCertificateIssuedEvent(&req)

	return certs, nil
}

type verifyLocksForUserCertsReq struct {
	checker services.AccessChecker

	// defaultMode is the default locking mode, as recorded in the cluster
	// Auth Preferences.
	defaultMode constants.LockingMode
	// username is the Teleport username.
	// Eg: tlsca.Identity.Username.
	username string
	// mfaVerified is the UUID of the MFA device used to authenticate the user.
	// Eg: tlsca.Identity.MFAVerified.
	mfaVerified string
	// activeAccessRequests are the UUIDs of active access requests for the user.
	// Eg: tlsca.Identity.ActiveRequests.
	activeAccessRequests []string
	// deviceID is the trusted device ID.
	// Eg: tlsca.Identity.DeviceExtensions.DeviceID
	deviceID string
}

// verifyLocksForUserCerts verifies if any locks are in place before issuing new
// user certificates.
func (a *Server) verifyLocksForUserCerts(req verifyLocksForUserCertsReq) error {
	checker := req.checker
	lockingMode := checker.LockingMode(req.defaultMode)

	lockTargets := []types.LockTarget{
		{User: req.username},
		{MFADevice: req.mfaVerified},
		{Device: req.deviceID},
	}
	lockTargets = append(lockTargets,
		services.RolesToLockTargets(checker.RoleNames())...,
	)
	lockTargets = append(lockTargets,
		services.AccessRequestsToLockTargets(req.activeAccessRequests)...,
	)

	return trace.Wrap(a.checkLockInForce(lockingMode, lockTargets))
}

// getSigningCAs returns the necessary resources to issue/sign new certificates.
func (a *Server) getSigningCAs(ctx context.Context, domainName string, caType types.CertAuthType) (*tlsca.CertAuthority, ssh.Signer, types.CertAuthority, error) {
	const loadKeys = true
	ca, err := a.GetCertAuthority(ctx, types.CertAuthID{
		Type:       caType,
		DomainName: domainName,
	}, loadKeys)
	if err != nil {
		return nil, nil, nil, trace.Wrap(err)
	}

	tlsCert, tlsSigner, err := a.keyStore.GetTLSCertAndSigner(ctx, ca)
	if err != nil {
		return nil, nil, nil, trace.Wrap(err)
	}
	tlsCA, err := tlsca.FromCertAndSigner(tlsCert, tlsSigner)
	if err != nil {
		return nil, nil, nil, trace.Wrap(err)
	}

	sshSigner, err := a.keyStore.GetSSHSigner(ctx, ca)
	if err != nil {
		return nil, nil, nil, trace.Wrap(err)
	}

	return tlsCA, sshSigner, ca, nil
}

// WithUserLock executes function authenticateFn that performs user authentication
// if authenticateFn returns non nil error, the login attempt will be logged in as failed.
// The only exception to this rule is ConnectionProblemError, in case if it occurs
// access will be denied, but login attempt will not be recorded
// this is done to avoid potential user lockouts due to backend failures
// In case if user exceeds defaults.MaxLoginAttempts
// the user account will be locked for defaults.AccountLockInterval
func (a *Server) WithUserLock(username string, authenticateFn func() error) error {
	user, err := a.Services.GetUser(username, false)
	if err != nil {
		if trace.IsNotFound(err) {
			// If user is not found, still call authenticateFn. It should
			// always return an error. This prevents username oracles and
			// timing attacks.
			return authenticateFn()
		}
		return trace.Wrap(err)
	}
	status := user.GetStatus()
	if status.IsLocked {
		if status.RecoveryAttemptLockExpires.After(a.clock.Now().UTC()) {
			log.Debugf("%v exceeds %v failed account recovery attempts, locked until %v",
				user.GetName(), defaults.MaxAccountRecoveryAttempts, apiutils.HumanTimeFormat(status.RecoveryAttemptLockExpires))

			err := trace.AccessDenied(MaxFailedAttemptsErrMsg)
			return trace.WithField(err, ErrFieldKeyUserMaxedAttempts, true)
		}
		if status.LockExpires.After(a.clock.Now().UTC()) {
			log.Debugf("%v exceeds %v failed login attempts, locked until %v",
				user.GetName(), defaults.MaxLoginAttempts, apiutils.HumanTimeFormat(status.LockExpires))

			err := trace.AccessDenied(MaxFailedAttemptsErrMsg)
			return trace.WithField(err, ErrFieldKeyUserMaxedAttempts, true)
		}
	}
	fnErr := authenticateFn()
	if fnErr == nil {
		// upon successful login, reset the failed attempt counter
		err = a.DeleteUserLoginAttempts(username)
		if !trace.IsNotFound(err) {
			return trace.Wrap(err)
		}

		return nil
	}
	// do not lock user in case if DB is flaky or down
	if trace.IsConnectionProblem(err) {
		return trace.Wrap(fnErr)
	}
	// log failed attempt and possibly lock user
	attempt := services.LoginAttempt{Time: a.clock.Now().UTC(), Success: false}
	err = a.AddUserLoginAttempt(username, attempt, defaults.AttemptTTL)
	if err != nil {
		log.Error(trace.DebugReport(err))
		return trace.Wrap(fnErr)
	}
	loginAttempts, err := a.GetUserLoginAttempts(username)
	if err != nil {
		log.Error(trace.DebugReport(err))
		return trace.Wrap(fnErr)
	}
	if !services.LastFailed(defaults.MaxLoginAttempts, loginAttempts) {
		log.Debugf("%v user has less than %v failed login attempts", username, defaults.MaxLoginAttempts)
		return trace.Wrap(fnErr)
	}
	lockUntil := a.clock.Now().UTC().Add(defaults.AccountLockInterval)
	log.Debug(fmt.Sprintf("%v exceeds %v failed login attempts, locked until %v",
		username, defaults.MaxLoginAttempts, apiutils.HumanTimeFormat(lockUntil)))
	user.SetLocked(lockUntil, "user has exceeded maximum failed login attempts")
	err = a.UpsertUser(user)
	if err != nil {
		log.Error(trace.DebugReport(err))
		return trace.Wrap(fnErr)
	}

	retErr := trace.AccessDenied(MaxFailedAttemptsErrMsg)
	return trace.WithField(retErr, ErrFieldKeyUserMaxedAttempts, true)
}

// PreAuthenticatedSignIn is for MFA authentication methods where the password
// is already checked before issuing the second factor challenge
func (a *Server) PreAuthenticatedSignIn(ctx context.Context, user string, identity tlsca.Identity) (types.WebSession, error) {
	accessInfo, err := services.AccessInfoFromLocalIdentity(identity, a)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	sess, err := a.NewWebSession(ctx, types.NewWebSessionRequest{
		User:                 user,
		LoginIP:              identity.LoginIP,
		Roles:                accessInfo.Roles,
		Traits:               accessInfo.Traits,
		AccessRequests:       identity.ActiveRequests,
		RequestedResourceIDs: accessInfo.AllowedResourceIDs,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if err := a.upsertWebSession(ctx, user, sess); err != nil {
		return nil, trace.Wrap(err)
	}
	return sess.WithoutSecrets(), nil
}

// CreateAuthenticateChallenge implements AuthService.CreateAuthenticateChallenge.
func (a *Server) CreateAuthenticateChallenge(ctx context.Context, req *proto.CreateAuthenticateChallengeRequest) (*proto.MFAAuthenticateChallenge, error) {
	var username string
	var passwordless bool

	switch req.GetRequest().(type) {
	case *proto.CreateAuthenticateChallengeRequest_UserCredentials:
		username = req.GetUserCredentials().GetUsername()

		if err := a.WithUserLock(username, func() error {
			return a.checkPasswordWOToken(username, req.GetUserCredentials().GetPassword())
		}); err != nil {
			return nil, trace.Wrap(err)
		}

	case *proto.CreateAuthenticateChallengeRequest_RecoveryStartTokenID:
		token, err := a.GetUserToken(ctx, req.GetRecoveryStartTokenID())
		if err != nil {
			log.Error(trace.DebugReport(err))
			return nil, trace.AccessDenied("invalid token")
		}

		if err := a.verifyUserToken(token, UserTokenTypeRecoveryStart); err != nil {
			return nil, trace.Wrap(err)
		}

		username = token.GetUser()

	case *proto.CreateAuthenticateChallengeRequest_Passwordless:
		passwordless = true // Allows empty username.

	default: // unset or CreateAuthenticateChallengeRequest_ContextUser.
		var err error
		username, err = authz.GetClientUsername(ctx)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}

	challenges, err := a.mfaAuthChallenge(ctx, username, passwordless)
	if err != nil {
		log.Error(trace.DebugReport(err))
		return nil, trace.AccessDenied("unable to create MFA challenges")
	}
	return challenges, nil
}

// CreateRegisterChallenge implements AuthService.CreateRegisterChallenge.
func (a *Server) CreateRegisterChallenge(ctx context.Context, req *proto.CreateRegisterChallengeRequest) (*proto.MFARegisterChallenge, error) {
	token, err := a.GetUserToken(ctx, req.GetTokenID())
	if err != nil {
		log.Error(trace.DebugReport(err))
		return nil, trace.AccessDenied("invalid token")
	}

	allowedTokenTypes := []string{
		UserTokenTypePrivilege,
		UserTokenTypePrivilegeException,
		UserTokenTypeResetPassword,
		UserTokenTypeResetPasswordInvite,
		UserTokenTypeRecoveryApproved,
	}

	if err := a.verifyUserToken(token, allowedTokenTypes...); err != nil {
		return nil, trace.AccessDenied("invalid token")
	}

	regChal, err := a.createRegisterChallenge(ctx, &newRegisterChallengeRequest{
		username:    token.GetUser(),
		token:       token,
		deviceType:  req.GetDeviceType(),
		deviceUsage: req.GetDeviceUsage(),
	})

	return regChal, trace.Wrap(err)
}

type newRegisterChallengeRequest struct {
	username    string
	deviceType  proto.DeviceType
	deviceUsage proto.DeviceUsage

	// token is a user token resource.
	// It is used as following:
	//  - TOTP:
	//    - create a UserTokenSecrets resource
	//    - store by token's ID using Server's IdentityService.
	//  - MFA:
	//    - store challenge by the token's ID
	//    - store by token's ID using Server's IdentityService.
	// This field can be empty to use storage overrides.
	token types.UserToken

	// webIdentityOverride is an optional RegistrationIdentity override to be used
	// to store webauthn challenge. A common override is decorating the regular
	// Identity with an in-memory SessionData storage.
	// Defaults to the Server's IdentityService.
	webIdentityOverride wanlib.RegistrationIdentity
}

func (a *Server) createRegisterChallenge(ctx context.Context, req *newRegisterChallengeRequest) (*proto.MFARegisterChallenge, error) {
	switch req.deviceType {
	case proto.DeviceType_DEVICE_TYPE_TOTP:
		otpKey, otpOpts, err := a.newTOTPKey(req.username)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		challenge := &proto.TOTPRegisterChallenge{
			Secret:        otpKey.Secret(),
			Issuer:        otpKey.Issuer(),
			PeriodSeconds: uint32(otpOpts.Period),
			Algorithm:     otpOpts.Algorithm.String(),
			Digits:        uint32(otpOpts.Digits.Length()),
			Account:       otpKey.AccountName(),
		}

		if req.token != nil {
			secrets, err := a.createTOTPUserTokenSecrets(ctx, req.token, otpKey)
			if err != nil {
				return nil, trace.Wrap(err)
			}

			challenge.QRCode = secrets.GetQRCode()
		}

		return &proto.MFARegisterChallenge{Request: &proto.MFARegisterChallenge_TOTP{TOTP: challenge}}, nil

	case proto.DeviceType_DEVICE_TYPE_WEBAUTHN:
		cap, err := a.GetAuthPreference(ctx)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		webConfig, err := cap.GetWebauthn()
		if err != nil {
			return nil, trace.Wrap(err)
		}

		identity := req.webIdentityOverride
		if identity == nil {
			identity = a.Services
		}

		webRegistration := &wanlib.RegistrationFlow{
			Webauthn: webConfig,
			Identity: identity,
		}

		passwordless := req.deviceUsage == proto.DeviceUsage_DEVICE_USAGE_PASSWORDLESS
		credentialCreation, err := webRegistration.Begin(ctx, req.username, passwordless)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		return &proto.MFARegisterChallenge{Request: &proto.MFARegisterChallenge_Webauthn{
			Webauthn: wantypes.CredentialCreationToProto(credentialCreation),
		}}, nil

	default:
		return nil, trace.BadParameter("MFA device type %q unsupported", req.deviceType.String())
	}
}

// GetMFADevices returns all mfa devices for the user defined in the token or the user defined in context.
func (a *Server) GetMFADevices(ctx context.Context, req *proto.GetMFADevicesRequest) (*proto.GetMFADevicesResponse, error) {
	var username string

	if req.GetTokenID() != "" {
		token, err := a.GetUserToken(ctx, req.GetTokenID())
		if err != nil {
			log.Error(trace.DebugReport(err))
			return nil, trace.AccessDenied("invalid token")
		}

		if err := a.verifyUserToken(token, UserTokenTypeRecoveryApproved); err != nil {
			return nil, trace.Wrap(err)
		}

		username = token.GetUser()
	}

	if username == "" {
		var err error
		username, err = authz.GetClientUsername(ctx)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}

	devs, err := a.Services.GetMFADevices(ctx, username, false)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return &proto.GetMFADevicesResponse{
		Devices: devs,
	}, nil
}

// DeleteMFADeviceSync implements AuthService.DeleteMFADeviceSync.
func (a *Server) DeleteMFADeviceSync(ctx context.Context, req *proto.DeleteMFADeviceSyncRequest) error {
	token, err := a.GetUserToken(ctx, req.GetTokenID())
	if err != nil {
		log.Error(trace.DebugReport(err))
		return trace.AccessDenied("invalid token")
	}

	if err := a.verifyUserToken(token, UserTokenTypeRecoveryApproved, UserTokenTypePrivilege); err != nil {
		return trace.Wrap(err)
	}

	_, err = a.deleteMFADeviceSafely(ctx, token.GetUser(), req.GetDeviceName())
	return trace.Wrap(err)
}

// deleteMFADeviceSafely deletes the user's mfa device while preventing users from deleting their last device
// for clusters that require second factors, which prevents users from being locked out of their account.
func (a *Server) deleteMFADeviceSafely(ctx context.Context, user, deviceName string) (*types.MFADevice, error) {
	devs, err := a.Services.GetMFADevices(ctx, user, true)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	authPref, err := a.GetAuthPreference(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	kindToSF := map[string]constants.SecondFactorType{
		fmt.Sprintf("%T", &types.MFADevice_Totp{}):     constants.SecondFactorOTP,
		fmt.Sprintf("%T", &types.MFADevice_U2F{}):      constants.SecondFactorWebauthn,
		fmt.Sprintf("%T", &types.MFADevice_Webauthn{}): constants.SecondFactorWebauthn,
	}
	sfToCount := make(map[constants.SecondFactorType]int)
	var knownDevices int
	var deviceToDelete *types.MFADevice

	// Find the device to delete and count devices.
	for _, d := range devs {
		// Match device by name or ID.
		if d.GetName() == deviceName || d.Id == deviceName {
			deviceToDelete = d
		}

		sf, ok := kindToSF[fmt.Sprintf("%T", d.Device)]
		switch {
		case !ok && d == deviceToDelete:
			return nil, trace.NotImplemented("cannot delete device of type %T", d.Device)
		case !ok:
			log.Warnf("Ignoring unknown device with type %T in deletion.", d.Device)
			continue
		}

		sfToCount[sf]++
		knownDevices++
	}
	if deviceToDelete == nil {
		return nil, trace.NotFound("MFA device %q does not exist", deviceName)
	}

	// Prevent users from deleting their last device for clusters that require second factors.
	const minDevices = 1
	switch sf := authPref.GetSecondFactor(); sf {
	case constants.SecondFactorOff, constants.SecondFactorOptional: // MFA is not required, allow deletion
	case constants.SecondFactorOn:
		if knownDevices <= minDevices {
			return nil, trace.BadParameter(
				"cannot delete the last MFA device for this user; add a replacement device first to avoid getting locked out")
		}
	case constants.SecondFactorOTP, constants.SecondFactorWebauthn:
		if sfToCount[sf] <= minDevices {
			return nil, trace.BadParameter(
				"cannot delete the last %s device for this user; add a replacement device first to avoid getting locked out", sf)
		}
	default:
		return nil, trace.BadParameter("unexpected second factor type: %s", sf)
	}

	if err := a.DeleteMFADevice(ctx, user, deviceToDelete.Id); err != nil {
		return nil, trace.Wrap(err)
	}

	// Emit deleted event.
	clusterName, err := a.GetClusterName()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if err := a.emitter.EmitAuditEvent(ctx, &apievents.MFADeviceDelete{
		Metadata: apievents.Metadata{
			Type:        events.MFADeviceDeleteEvent,
			Code:        events.MFADeviceDeleteEventCode,
			ClusterName: clusterName.GetClusterName(),
		},
		UserMetadata:      authz.ClientUserMetadataWithUser(ctx, user),
		MFADeviceMetadata: mfaDeviceEventMetadata(deviceToDelete),
	}); err != nil {
		return nil, trace.Wrap(err)
	}
	return deviceToDelete, nil
}

// AddMFADeviceSync implements AuthService.AddMFADeviceSync.
func (a *Server) AddMFADeviceSync(ctx context.Context, req *proto.AddMFADeviceSyncRequest) (*proto.AddMFADeviceSyncResponse, error) {
	privilegeToken, err := a.GetUserToken(ctx, req.GetTokenID())
	if err != nil {
		log.Error(trace.DebugReport(err))
		return nil, trace.AccessDenied("invalid token")
	}

	if err := a.verifyUserToken(privilegeToken, UserTokenTypePrivilege, UserTokenTypePrivilegeException); err != nil {
		return nil, trace.Wrap(err)
	}

	dev, err := a.verifyMFARespAndAddDevice(ctx, &newMFADeviceFields{
		username:      privilegeToken.GetUser(),
		newDeviceName: req.GetNewDeviceName(),
		tokenID:       privilegeToken.GetName(),
		deviceResp:    req.GetNewMFAResponse(),
		deviceUsage:   req.DeviceUsage,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return &proto.AddMFADeviceSyncResponse{Device: dev}, nil
}

type newMFADeviceFields struct {
	username      string
	newDeviceName string
	// tokenID is the ID of a reset/invite/recovery token.
	// It is used as following:
	//  - TOTP:
	//    - look up TOTP secret stored by token ID
	//  - MFA:
	//    - look up challenge stored by token ID
	// This field can be empty to use storage overrides.
	tokenID string
	// totpSecret is a secret shared by client and server to generate totp codes.
	// Field can be empty to get secret by "tokenID".
	totpSecret string

	// webIdentityOverride is an optional RegistrationIdentity override to be used
	// for device registration. A common override is decorating the regular
	// Identity with an in-memory SessionData storage.
	// Defaults to the Server's IdentityService.
	webIdentityOverride wanlib.RegistrationIdentity
	// deviceResp is the register response from the new device.
	deviceResp *proto.MFARegisterResponse
	// deviceUsage describes the intended usage of the new device.
	deviceUsage proto.DeviceUsage
}

// verifyMFARespAndAddDevice validates MFA register response and on success adds the new MFA device.
func (a *Server) verifyMFARespAndAddDevice(ctx context.Context, req *newMFADeviceFields) (*types.MFADevice, error) {
	if len(req.newDeviceName) > mfaDeviceNameMaxLen {
		return nil, trace.BadParameter("device name must be %v characters or less", mfaDeviceNameMaxLen)
	}

	cap, err := a.GetAuthPreference(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if cap.GetSecondFactor() == constants.SecondFactorOff {
		return nil, trace.BadParameter("second factor disabled by cluster configuration")
	}

	var dev *types.MFADevice
	switch req.deviceResp.GetResponse().(type) {
	case *proto.MFARegisterResponse_TOTP:
		dev, err = a.registerTOTPDevice(ctx, req.deviceResp, req)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	case *proto.MFARegisterResponse_Webauthn:
		dev, err = a.registerWebauthnDevice(ctx, req.deviceResp, req)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	default:
		return nil, trace.BadParameter("MFARegisterResponse is an unknown response type %T", req.deviceResp.Response)
	}

	clusterName, err := a.GetClusterName()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if err := a.emitter.EmitAuditEvent(ctx, &apievents.MFADeviceAdd{
		Metadata: apievents.Metadata{
			Type:        events.MFADeviceAddEvent,
			Code:        events.MFADeviceAddEventCode,
			ClusterName: clusterName.GetClusterName(),
		},
		UserMetadata:      authz.ClientUserMetadataWithUser(ctx, req.username),
		MFADeviceMetadata: mfaDeviceEventMetadata(dev),
	}); err != nil {
		log.WithError(err).Warn("Failed to emit add mfa device event.")
	}

	return dev, nil
}

func (a *Server) registerTOTPDevice(ctx context.Context, regResp *proto.MFARegisterResponse, req *newMFADeviceFields) (*types.MFADevice, error) {
	cap, err := a.GetAuthPreference(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if !cap.IsSecondFactorTOTPAllowed() {
		return nil, trace.BadParameter("second factor TOTP not allowed by cluster")
	}

	var secret string
	switch {
	case req.tokenID != "":
		secrets, err := a.GetUserTokenSecrets(ctx, req.tokenID)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		secret = secrets.GetOTPKey()
	case req.totpSecret != "":
		secret = req.totpSecret
	default:
		return nil, trace.BadParameter("missing TOTP secret")
	}

	dev, err := services.NewTOTPDevice(req.newDeviceName, secret, a.clock.Now())
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if err := a.checkTOTP(ctx, req.username, regResp.GetTOTP().GetCode(), dev); err != nil {
		return nil, trace.Wrap(err)
	}
	if err := a.UpsertMFADevice(ctx, req.username, dev); err != nil {
		return nil, trace.Wrap(err)
	}
	return dev, nil
}

func (a *Server) registerWebauthnDevice(ctx context.Context, regResp *proto.MFARegisterResponse, req *newMFADeviceFields) (*types.MFADevice, error) {
	cap, err := a.GetAuthPreference(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if !cap.IsSecondFactorWebauthnAllowed() {
		return nil, trace.BadParameter("second factor webauthn not allowed by cluster")
	}

	webConfig, err := cap.GetWebauthn()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	identity := req.webIdentityOverride // Override Identity, if supplied.
	if identity == nil {
		identity = a.Services
	}
	webRegistration := &wanlib.RegistrationFlow{
		Webauthn: webConfig,
		Identity: identity,
	}
	// Finish upserts the device on success.
	dev, err := webRegistration.Finish(ctx, wanlib.RegisterResponse{
		User:             req.username,
		DeviceName:       req.newDeviceName,
		CreationResponse: wantypes.CredentialCreationResponseFromProto(regResp.GetWebauthn()),
		Passwordless:     req.deviceUsage == proto.DeviceUsage_DEVICE_USAGE_PASSWORDLESS,
	})
	return dev, trace.Wrap(err)
}

// GetWebSession returns existing web session described by req. Explicitly
// delegating to Services as it's directly implemented by Cache as well.
func (a *Server) GetWebSession(ctx context.Context, req types.GetWebSessionRequest) (types.WebSession, error) {
	return a.Services.GetWebSession(ctx, req)
}

// GetWebToken returns existing web token described by req. Explicitly
// delegating to Services as it's directly implemented by Cache as well.
func (a *Server) GetWebToken(ctx context.Context, req types.GetWebTokenRequest) (types.WebToken, error) {
	return a.Services.GetWebToken(ctx, req)
}

// ExtendWebSession creates a new web session for a user based on a valid previous (current) session.
//
// If there is an approved access request, additional roles are appended to the roles that were
// extracted from identity. The new session expiration time will not exceed the expiration time
// of the previous session.
//
// If there is a switchback request, the roles will switchback to user's default roles and
// the expiration time is derived from users recently logged in time.
func (a *Server) ExtendWebSession(ctx context.Context, req WebSessionReq, identity tlsca.Identity) (types.WebSession, error) {
	prevSession, err := a.GetWebSession(ctx, types.GetWebSessionRequest{
		User:      req.User,
		SessionID: req.PrevSessionID,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// consider absolute expiry time that may be set for this session
	// by some external identity service, so we can not renew this session
	// anymore without extra logic for renewal with external OIDC provider
	expiresAt := prevSession.GetExpiryTime()
	if !expiresAt.IsZero() && expiresAt.Before(a.clock.Now().UTC()) {
		return nil, trace.NotFound("web session has expired")
	}

	accessInfo, err := services.AccessInfoFromLocalIdentity(identity, a)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	roles := accessInfo.Roles
	traits := accessInfo.Traits
	allowedResourceIDs := accessInfo.AllowedResourceIDs
	accessRequests := identity.ActiveRequests

	if req.ReloadUser {
		// We don't call from the cache layer because we want to
		// retrieve the recently updated user. Otherwise, the cache
		// returns stale data.
		user, err := a.Identity.GetUser(req.User, false)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		traits = user.GetTraits()

	} else if req.AccessRequestID != "" {
		accessRequest, err := a.getValidatedAccessRequest(ctx, identity, req.User, req.AccessRequestID)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		roles = append(roles, accessRequest.GetRoles()...)
		roles = apiutils.Deduplicate(roles)
		accessRequests = apiutils.Deduplicate(append(accessRequests, req.AccessRequestID))

		if len(accessRequest.GetRequestedResourceIDs()) > 0 {
			// There's not a consistent way to merge multiple resource access
			// requests, a user may be able to request access to different resources
			// with different roles which should not overlap.
			if len(allowedResourceIDs) > 0 {
				return nil, trace.BadParameter("user is already logged in with a resource access request, cannot assume another")
			}
			allowedResourceIDs = accessRequest.GetRequestedResourceIDs()
		}

		webSessionTTL := a.getWebSessionTTL(accessRequest)

		// Let the session expire with the shortest expiry time.
		if expiresAt.After(webSessionTTL) {
			expiresAt = webSessionTTL
		}
	} else if req.Switchback {
		if prevSession.GetLoginTime().IsZero() {
			return nil, trace.BadParameter("Unable to switchback, log in time was not recorded.")
		}

		// Get default/static roles.
		user, err := a.GetUser(req.User, false)
		if err != nil {
			return nil, trace.Wrap(err, "failed to switchback")
		}

		// Reset any search-based access requests
		allowedResourceIDs = nil

		// Calculate expiry time.
		roleSet, err := services.FetchRoles(user.GetRoles(), a, user.GetTraits())
		if err != nil {
			return nil, trace.Wrap(err)
		}

		sessionTTL := roleSet.AdjustSessionTTL(apidefaults.CertDuration)

		// Set default roles and expiration.
		expiresAt = prevSession.GetLoginTime().UTC().Add(sessionTTL)
		roles = user.GetRoles()
		accessRequests = nil
	}

	sessionTTL := utils.ToTTL(a.clock, expiresAt)
	sess, err := a.NewWebSession(ctx, types.NewWebSessionRequest{
		User:                 req.User,
		LoginIP:              identity.LoginIP,
		Roles:                roles,
		Traits:               traits,
		SessionTTL:           sessionTTL,
		AccessRequests:       accessRequests,
		RequestedResourceIDs: allowedResourceIDs,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Keep preserving the login time.
	sess.SetLoginTime(prevSession.GetLoginTime())

	sess.SetConsumedAccessRequestID(req.AccessRequestID)

	if err := a.upsertWebSession(ctx, req.User, sess); err != nil {
		return nil, trace.Wrap(err)
	}

	return sess, nil
}

// getWebSessionTTL returns the earliest expiration time of allowed in the access request.
func (a *Server) getWebSessionTTL(accessRequest types.AccessRequest) time.Time {
	webSessionTTL := accessRequest.GetAccessExpiry()
	sessionTTL := accessRequest.GetSessionTLL()
	if sessionTTL.IsZero() {
		return webSessionTTL
	}

	// Session TTL contains the time when the session should end.
	// We need to subtract it from the creation time to get the
	// session duration.
	sessionDuration := sessionTTL.Sub(accessRequest.GetCreationTime())
	// Calculate the adjusted session TTL.
	adjustedSessionTTL := a.clock.Now().UTC().Add(sessionDuration)
	// Adjusted TTL can't exceed webSessionTTL.
	if adjustedSessionTTL.Before(webSessionTTL) {
		return adjustedSessionTTL
	}
	return webSessionTTL
}

func (a *Server) getValidatedAccessRequest(ctx context.Context, identity tlsca.Identity, user string, accessRequestID string) (types.AccessRequest, error) {
	reqFilter := types.AccessRequestFilter{
		User: user,
		ID:   accessRequestID,
	}

	reqs, err := a.GetAccessRequests(ctx, reqFilter)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if len(reqs) < 1 {
		return nil, trace.NotFound("access request %q not found", accessRequestID)
	}

	req := reqs[0]

	if !req.GetState().IsApproved() {
		if req.GetState().IsDenied() {
			return nil, trace.AccessDenied("access request %q has been denied", accessRequestID)
		}
		return nil, trace.AccessDenied("access request %q is awaiting approval", accessRequestID)
	}

	if err := services.ValidateAccessRequestForUser(ctx, a.clock, a, req, identity); err != nil {
		return nil, trace.Wrap(err)
	}

	accessExpiry := req.GetAccessExpiry()
	if accessExpiry.Before(a.GetClock().Now()) {
		return nil, trace.BadParameter("access request %q has expired", accessRequestID)
	}

	return req, nil
}

// CreateWebSession creates a new web session for user without any
// checks, is used by admins
func (a *Server) CreateWebSession(ctx context.Context, user string) (types.WebSession, error) {
	u, err := a.getUserOrLoginState(ctx, user)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	session, err := a.CreateWebSessionFromReq(ctx, types.NewWebSessionRequest{
		User:      user,
		Roles:     u.GetRoles(),
		Traits:    u.GetTraits(),
		LoginTime: a.clock.Now().UTC(),
	})
	return session, trace.Wrap(err)
}

// ExtractHostID returns host id based on the hostname
func ExtractHostID(hostName string, clusterName string) (string, error) {
	suffix := "." + clusterName
	if !strings.HasSuffix(hostName, suffix) {
		return "", trace.BadParameter("expected suffix %q in %q", suffix, hostName)
	}
	return strings.TrimSuffix(hostName, suffix), nil
}

// HostFQDN consists of host UUID and cluster name joined via .
func HostFQDN(hostUUID, clusterName string) string {
	return fmt.Sprintf("%v.%v", hostUUID, clusterName)
}

// GenerateHostCerts generates new host certificates (signed
// by the host certificate authority) for a node.
func (a *Server) GenerateHostCerts(ctx context.Context, req *proto.HostCertsRequest) (*proto.Certs, error) {
	if err := req.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}

	if err := req.Role.Check(); err != nil {
		return nil, err
	}

	if err := a.limiter.AcquireConnection(req.Role.String()); err != nil {
		generateThrottledRequestsCount.Inc()
		log.Debugf("Node %q [%v] is rate limited: %v.", req.NodeName, req.HostID, req.Role)
		return nil, trace.Wrap(err)
	}
	defer a.limiter.ReleaseConnection(req.Role.String())

	// only observe latencies for non-throttled requests
	start := a.clock.Now()
	defer func() { generateRequestsLatencies.Observe(time.Since(start).Seconds()) }()

	generateRequestsCount.Inc()
	generateRequestsCurrent.Inc()
	defer generateRequestsCurrent.Dec()

	clusterName, err := a.GetClusterName()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// If the request contains 0.0.0.0, this implies an advertise IP was not
	// specified on the node. Try and guess what the address by replacing 0.0.0.0
	// with the RemoteAddr as known to the Auth Server.
	if slices.Contains(req.AdditionalPrincipals, defaults.AnyAddress) {
		remoteHost, err := utils.Host(req.RemoteAddr)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		req.AdditionalPrincipals = utils.ReplaceInSlice(
			req.AdditionalPrincipals,
			defaults.AnyAddress,
			remoteHost)
	}

	if _, _, _, _, err := ssh.ParseAuthorizedKey(req.PublicSSHKey); err != nil {
		return nil, trace.BadParameter("failed to parse SSH public key")
	}
	cryptoPubKey, err := tlsca.ParsePublicKeyPEM(req.PublicTLSKey)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// get the certificate authority that will be signing the public key of the host,
	client := a.Cache
	if req.NoCache {
		client = a.Services
	}
	ca, err := client.GetCertAuthority(ctx, types.CertAuthID{
		Type:       types.HostCA,
		DomainName: clusterName.GetClusterName(),
	}, true)
	if err != nil {
		return nil, trace.BadParameter("failed to load host CA for %q: %v", clusterName.GetClusterName(), err)
	}

	// could be a couple of scenarios, either client data is out of sync,
	// or auth server is out of sync, either way, for now check that
	// cache is out of sync, this will result in higher read rate
	// to the backend, which is a fine tradeoff
	if !req.NoCache && !req.Rotation.IsZero() && !req.Rotation.Matches(ca.GetRotation()) {
		log.Debugf("Client sent rotation state %v, cache state is %v, using state from the DB.", req.Rotation, ca.GetRotation())
		ca, err = a.Services.GetCertAuthority(ctx, types.CertAuthID{
			Type:       types.HostCA,
			DomainName: clusterName.GetClusterName(),
		}, true)
		if err != nil {
			return nil, trace.BadParameter("failed to load host CA for %q: %v", clusterName.GetClusterName(), err)
		}
		if !req.Rotation.Matches(ca.GetRotation()) {
			return nil, trace.BadParameter(""+
				"the client expected state is out of sync, server rotation state: %v, "+
				"client rotation state: %v, re-register the client from scratch to fix the issue.",
				ca.GetRotation(), req.Rotation)
		}
	}

	isAdminRole := req.Role == types.RoleAdmin

	cert, signer, err := a.keyStore.GetTLSCertAndSigner(ctx, ca)
	if trace.IsNotFound(err) && isAdminRole {
		// If there is no local TLS signer found in the host CA ActiveKeys, this
		// auth server may have a newly configured HSM and has only populated
		// local keys in the AdditionalTrustedKeys until the next CA rotation.
		// This is the only case where we should be able to get a signer from
		// AdditionalTrustedKeys but not ActiveKeys.
		cert, signer, err = a.keyStore.GetAdditionalTrustedTLSCertAndSigner(ctx, ca)
	}
	if err != nil {
		return nil, trace.Wrap(err)
	}
	tlsAuthority, err := tlsca.FromCertAndSigner(cert, signer)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	caSigner, err := a.keyStore.GetSSHSigner(ctx, ca)
	if trace.IsNotFound(err) && isAdminRole {
		// If there is no local SSH signer found in the host CA ActiveKeys, this
		// auth server may have a newly configured HSM and has only populated
		// local keys in the AdditionalTrustedKeys until the next CA rotation.
		// This is the only case where we should be able to get a signer from
		// AdditionalTrustedKeys but not ActiveKeys.
		caSigner, err = a.keyStore.GetAdditionalTrustedSSHSigner(ctx, ca)
	}
	if err != nil {
		return nil, trace.Wrap(err)
	}
	// generate host SSH certificate
	hostSSHCert, err := a.generateHostCert(ctx, services.HostCertParams{
		CASigner:      caSigner,
		PublicHostKey: req.PublicSSHKey,
		HostID:        req.HostID,
		NodeName:      req.NodeName,
		ClusterName:   clusterName.GetClusterName(),
		Role:          req.Role,
		Principals:    req.AdditionalPrincipals,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if req.Role == types.RoleInstance && len(req.SystemRoles) == 0 {
		return nil, trace.BadParameter("cannot generate instance cert with no system roles")
	}

	systemRoles := make([]string, 0, len(req.SystemRoles))
	for _, r := range req.SystemRoles {
		systemRoles = append(systemRoles, string(r))
	}

	// generate host TLS certificate
	identity := tlsca.Identity{
		Username:        HostFQDN(req.HostID, clusterName.GetClusterName()),
		Groups:          []string{req.Role.String()},
		TeleportCluster: clusterName.GetClusterName(),
		SystemRoles:     systemRoles,
	}
	subject, err := identity.Subject()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	certRequest := tlsca.CertificateRequest{
		Clock:     a.clock,
		PublicKey: cryptoPubKey,
		Subject:   subject,
		NotAfter:  a.clock.Now().UTC().Add(defaults.CATTL),
		DNSNames:  append([]string{}, req.AdditionalPrincipals...),
	}

	// API requests need to specify a DNS name, which must be present in the certificate's DNS Names.
	// The target DNS is not always known in advance, so we add a default one to all certificates.
	certRequest.DNSNames = append(certRequest.DNSNames, DefaultDNSNamesForRole(req.Role)...)
	// Unlike additional principals, DNS Names is x509 specific and is limited
	// to services with TLS endpoints (e.g. auth, proxies, kubernetes)
	if (types.SystemRoles{req.Role}).IncludeAny(types.RoleAuth, types.RoleAdmin, types.RoleProxy, types.RoleKube, types.RoleWindowsDesktop) {
		certRequest.DNSNames = append(certRequest.DNSNames, req.DNSNames...)
	}
	hostTLSCert, err := tlsAuthority.GenerateCertificate(certRequest)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return &proto.Certs{
		SSH:        hostSSHCert,
		TLS:        hostTLSCert,
		TLSCACerts: services.GetTLSCerts(ca),
		SSHCACerts: services.GetSSHCheckingKeys(ca),
	}, nil
}

func (a *Server) RegisterInventoryControlStream(ics client.UpstreamInventoryControlStream, hello proto.UpstreamInventoryHello) error {
	// upstream hello is pulled and checked at rbac layer. we wait to send the downstream hello until we get here
	// in order to simplify creation of in-memory streams when dealing with local auth (note: in theory we could
	// send hellos simultaneously to slightly improve perf, but there is a potential benefit to having the
	// downstream hello serve double-duty as an indicator of having successfully transitioned the rbac layer).
	downstreamHello := proto.DownstreamInventoryHello{
		Version:  teleport.Version,
		ServerID: a.ServerID,
	}
	if err := ics.Send(a.CloseContext(), downstreamHello); err != nil {
		return trace.Wrap(err)
	}
	a.inventory.RegisterControlStream(ics, hello)
	return nil
}

// MakeLocalInventoryControlStream sets up an in-memory control stream which automatically registers with this auth
// server upon hello exchange.
func (a *Server) MakeLocalInventoryControlStream(opts ...client.ICSPipeOption) client.DownstreamInventoryControlStream {
	upstream, downstream := client.InventoryControlStreamPipe(opts...)
	go func() {
		select {
		case msg := <-upstream.Recv():
			hello, ok := msg.(proto.UpstreamInventoryHello)
			if !ok {
				upstream.CloseWithError(trace.BadParameter("expected upstream hello, got: %T", msg))
				return
			}
			if err := a.RegisterInventoryControlStream(upstream, hello); err != nil {
				upstream.CloseWithError(err)
				return
			}
		case <-upstream.Done():
		case <-a.CloseContext().Done():
			upstream.Close()
		}
	}()
	return downstream
}

func (a *Server) GetInventoryStatus(ctx context.Context, req proto.InventoryStatusRequest) (proto.InventoryStatusSummary, error) {
	var rsp proto.InventoryStatusSummary
	if req.Connected {
		a.inventory.Iter(func(handle inventory.UpstreamHandle) {
			rsp.Connected = append(rsp.Connected, handle.Hello())
		})

		// connected instance list is a special case, don't bother aggregating heartbeats
		return rsp, nil
	}

	rsp.VersionCounts = make(map[string]uint32)
	rsp.UpgraderCounts = make(map[string]uint32)
	rsp.ServiceCounts = make(map[string]uint32)

	ins := a.GetInstances(ctx, types.InstanceFilter{})

	for ins.Next() {
		rsp.InstanceCount++

		rsp.VersionCounts[vc.Normalize(ins.Item().GetTeleportVersion())]++

		upgrader := ins.Item().GetExternalUpgrader()
		if upgrader == "" {
			upgrader = "none"
		}

		rsp.UpgraderCounts[upgrader]++

		for _, service := range ins.Item().GetServices() {
			rsp.ServiceCounts[string(service)]++
		}
	}

	return rsp, ins.Done()
}

// GetInventoryConnectedServiceCounts returns the counts of each connected service seen in the inventory.
func (a *Server) GetInventoryConnectedServiceCounts() proto.InventoryConnectedServiceCounts {
	return proto.InventoryConnectedServiceCounts{
		ServiceCounts: a.inventory.ConnectedServiceCounts(),
	}
}

// GetInventoryConnectedServiceCount returns the counts of a particular connected service seen in the inventory.
func (a *Server) GetInventoryConnectedServiceCount(service types.SystemRole) uint64 {
	return a.inventory.ConnectedServiceCount(service)
}

func (a *Server) PingInventory(ctx context.Context, req proto.InventoryPingRequest) (proto.InventoryPingResponse, error) {
	const pingAttempt = "ping-attempt"
	const pingSuccess = "ping-success"
	const maxAttempts = 16
	stream, ok := a.inventory.GetControlStream(req.ServerID)
	if !ok {
		return proto.InventoryPingResponse{}, trace.NotFound("no control stream found for server %q", req.ServerID)
	}

	id := insecurerand.Uint64()

	if !req.ControlLog {
		// this ping doesn't pass through the control log, so just execute it immediately.
		d, err := stream.Ping(ctx, id)
		return proto.InventoryPingResponse{
			Duration: d,
		}, trace.Wrap(err)
	}

	// matchEntry is used to check if our log entry has been included
	// in the control log.
	matchEntry := func(entry types.InstanceControlLogEntry) bool {
		return entry.Type == pingAttempt && entry.ID == id
	}

	var included bool
	for i := 1; i <= maxAttempts; i++ {
		stream.VisitInstanceState(func(ref inventory.InstanceStateRef) (update inventory.InstanceStateUpdate) {
			// check if we've already successfully included the ping entry
			if ref.LastHeartbeat != nil {
				if slices.IndexFunc(ref.LastHeartbeat.GetControlLog(), matchEntry) >= 0 {
					included = true
					return
				}
			}

			// if the entry pending already, we just need to wait
			if slices.IndexFunc(ref.QualifiedPendingControlLog, matchEntry) >= 0 {
				return
			}

			// either this is the first iteration, or the pending control log was reset.
			update.QualifiedPendingControlLog = append(update.QualifiedPendingControlLog, types.InstanceControlLogEntry{
				Type: pingAttempt,
				ID:   id,
				Time: time.Now(),
			})
			stream.HeartbeatInstance()
			return
		})

		if included {
			// entry appeared in control log
			break
		}

		// pause briefly, then re-sync our state. note that this strategy is not scalable. control log usage is intended only
		// for periodic operations. control-log based pings are a mechanism for testing/debugging only, hence the use of a
		// simple sleep loop.
		select {
		case <-time.After(time.Millisecond * 100 * time.Duration(i)):
		case <-stream.Done():
			return proto.InventoryPingResponse{}, trace.Errorf("control stream closed during ping attempt")
		case <-ctx.Done():
			return proto.InventoryPingResponse{}, trace.Wrap(ctx.Err())
		}
	}

	if !included {
		return proto.InventoryPingResponse{}, trace.LimitExceeded("failed to include ping %d in control log for instance %q (max attempts exceeded)", id, req.ServerID)
	}

	d, err := stream.Ping(ctx, id)
	if err != nil {
		return proto.InventoryPingResponse{}, trace.Wrap(err)
	}

	stream.VisitInstanceState(func(_ inventory.InstanceStateRef) (update inventory.InstanceStateUpdate) {
		update.UnqualifiedPendingControlLog = append(update.UnqualifiedPendingControlLog, types.InstanceControlLogEntry{
			Type: pingSuccess,
			ID:   id,
			Labels: map[string]string{
				"duration": d.String(),
			},
		})
		return
	})
	stream.HeartbeatInstance()

	return proto.InventoryPingResponse{
		Duration: d,
	}, nil
}

// UpdateLabels updates the labels on an instance over the inventory control
// stream.
func (a *Server) UpdateLabels(ctx context.Context, req proto.InventoryUpdateLabelsRequest) error {
	stream, ok := a.inventory.GetControlStream(req.ServerID)
	if !ok {
		return trace.NotFound("no control stream found for server %q", req.ServerID)
	}
	return trace.Wrap(stream.UpdateLabels(ctx, req.Kind, req.Labels))
}

// TokenExpiredOrNotFound is a special message returned by the auth server when provisioning
// tokens are either past their TTL, or could not be found.
const TokenExpiredOrNotFound = "token expired or not found"

// ValidateToken takes a provisioning token value and finds if it's valid. Returns
// a list of roles this token allows its owner to assume and token labels, or an error if the token
// cannot be found.
func (a *Server) ValidateToken(ctx context.Context, token string) (types.ProvisionToken, error) {
	tkns, err := a.GetStaticTokens()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// First check if the token is a static token. If it is, return right away.
	// Static tokens have no expiration.
	for _, st := range tkns.GetStaticTokens() {
		if subtle.ConstantTimeCompare([]byte(st.GetName()), []byte(token)) == 1 {
			return st, nil
		}
	}

	// If it's not a static token, check if it's a ephemeral token in the backend.
	// If a ephemeral token is found, make sure it's still valid.
	tok, err := a.GetToken(ctx, token)
	if err != nil {
		if trace.IsNotFound(err) {
			return nil, trace.AccessDenied(TokenExpiredOrNotFound)
		}
		return nil, trace.Wrap(err)
	}
	if !a.checkTokenTTL(tok) {
		return nil, trace.AccessDenied(TokenExpiredOrNotFound)
	}

	return tok, nil
}

// checkTokenTTL checks if the token is still valid. If it is not, the token
// is removed from the backend and returns false. Otherwise returns true.
func (a *Server) checkTokenTTL(tok types.ProvisionToken) bool {
	// Always accept tokens without an expiry configured.
	if tok.Expiry().IsZero() {
		return true
	}

	now := a.clock.Now().UTC()
	if tok.Expiry().Before(now) {
		// Tidy up the expired token in background if it has expired.
		go func() {
			ctx, cancel := context.WithTimeout(a.CloseContext(), time.Second*30)
			defer cancel()
			if err := a.DeleteToken(ctx, tok.GetName()); err != nil {
				if !trace.IsNotFound(err) {
					log.Warnf("Unable to delete token from backend: %v.", err)
				}
			}
		}()
		return false
	}
	return true
}

func (a *Server) DeleteToken(ctx context.Context, token string) (err error) {
	tkns, err := a.GetStaticTokens()
	if err != nil {
		return trace.Wrap(err)
	}

	// is this a static token?
	for _, st := range tkns.GetStaticTokens() {
		if subtle.ConstantTimeCompare([]byte(st.GetName()), []byte(token)) == 1 {
			return trace.BadParameter("token %s is statically configured and cannot be removed", backend.MaskKeyName(token))
		}
	}
	// Delete a user token.
	if err = a.DeleteUserToken(ctx, token); err == nil {
		return nil
	}
	// delete node token:
	if err = a.Services.DeleteToken(ctx, token); err == nil {
		return nil
	}
	return trace.Wrap(err)
}

// GetTokens returns all tokens (machine provisioning ones and user tokens). Machine
// tokens usually have "node roles", like auth,proxy,node and user invitation tokens have 'signup' role
func (a *Server) GetTokens(ctx context.Context, opts ...services.MarshalOption) (tokens []types.ProvisionToken, err error) {
	// get node tokens:
	tokens, err = a.Services.GetTokens(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	// get static tokens:
	tkns, err := a.GetStaticTokens()
	if err != nil && !trace.IsNotFound(err) {
		return nil, trace.Wrap(err)
	}
	if err == nil {
		tokens = append(tokens, tkns.GetStaticTokens()...)
	}
	// get user tokens:
	userTokens, err := a.GetUserTokens(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	// convert user tokens to machine tokens:
	for _, t := range userTokens {
		roles := types.SystemRoles{types.RoleSignup}
		tok, err := types.NewProvisionToken(t.GetName(), roles, t.Expiry())
		if err != nil {
			return nil, trace.Wrap(err)
		}
		tokens = append(tokens, tok)
	}
	return tokens, nil
}

// NewWebSession creates and returns a new web session for the specified request
func (a *Server) NewWebSession(ctx context.Context, req types.NewWebSessionRequest) (types.WebSession, error) {
	userState, err := a.getUserOrLoginState(ctx, req.User)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if req.LoginIP == "" {
		// TODO(antonam): consider turning this into error after all use cases are covered (before v14.0 testplan)
		log.Debug("Creating new web session without login IP specified.")
	}
	clusterName, err := a.GetClusterName()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	checker, err := services.NewAccessChecker(&services.AccessInfo{
		Roles:              req.Roles,
		Traits:             req.Traits,
		AllowedResourceIDs: req.RequestedResourceIDs,
	}, clusterName.GetClusterName(), a)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	netCfg, err := a.GetClusterNetworkingConfig(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	priv, pub, err := native.GenerateKeyPair()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	sessionTTL := req.SessionTTL
	if sessionTTL == 0 {
		sessionTTL = checker.AdjustSessionTTL(apidefaults.CertDuration)
	}
	certs, err := a.generateUserCert(certRequest{
		user:           userState,
		loginIP:        req.LoginIP,
		ttl:            sessionTTL,
		publicKey:      pub,
		checker:        checker,
		traits:         req.Traits,
		activeRequests: services.RequestIDs{AccessRequests: req.AccessRequests},
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	token, err := utils.CryptoRandomHex(SessionTokenBytes)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	bearerToken, err := utils.CryptoRandomHex(SessionTokenBytes)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	bearerTokenTTL := utils.MinTTL(sessionTTL, BearerTokenTTL)

	startTime := a.clock.Now()
	if !req.LoginTime.IsZero() {
		startTime = req.LoginTime
	}

	sessionSpec := types.WebSessionSpecV2{
		User:               req.User,
		Priv:               priv,
		Pub:                certs.SSH,
		TLSCert:            certs.TLS,
		Expires:            startTime.UTC().Add(sessionTTL),
		BearerToken:        bearerToken,
		BearerTokenExpires: startTime.UTC().Add(bearerTokenTTL),
		LoginTime:          req.LoginTime,
		IdleTimeout:        types.Duration(netCfg.GetWebIdleTimeout()),
	}
	UserLoginCount.Inc()

	sess, err := types.NewWebSession(token, types.KindWebSession, sessionSpec)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return sess, nil
}

// GetWebSessionInfo returns the web session specified with sessionID for the given user.
// The session is stripped of any authentication details.
// Implements auth.WebUIService
func (a *Server) GetWebSessionInfo(ctx context.Context, user, sessionID string) (types.WebSession, error) {
	sess, err := a.GetWebSession(ctx, types.GetWebSessionRequest{User: user, SessionID: sessionID})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return sess.WithoutSecrets(), nil
}

func (a *Server) DeleteNamespace(namespace string) error {
	ctx := context.TODO()
	if namespace == apidefaults.Namespace {
		return trace.AccessDenied("can't delete default namespace")
	}
	nodes, err := a.GetNodes(ctx, namespace)
	if err != nil {
		return trace.Wrap(err)
	}
	if len(nodes) != 0 {
		return trace.BadParameter("can't delete namespace %v that has %v registered nodes", namespace, len(nodes))
	}
	return a.Services.DeleteNamespace(namespace)
}
func (a *Server) CreateAccessRequest(ctx context.Context, req types.AccessRequest, identity tlsca.Identity) error {
	_, err := a.CreateAccessRequestV2(ctx, req, identity)
	return trace.Wrap(err)
}

func (a *Server) CreateAccessRequestV2(ctx context.Context, req types.AccessRequest, identity tlsca.Identity) (types.AccessRequest, error) {
	now := a.clock.Now().UTC()

	req.SetCreationTime(now)

	// Always perform variable expansion on creation only; this ensures the
	// access request that is reviewed is the same that is approved.
	expandOpts := services.ExpandVars(true)
	if err := services.ValidateAccessRequestForUser(ctx, a.clock, a, req, identity, expandOpts); err != nil {
		return nil, trace.Wrap(err)
	}

	// Look for user groups and associated applications to the request.
	requestedResourceIDs := req.GetRequestedResourceIDs()
	var additionalResources []types.ResourceID

	var userGroups []types.ResourceID
	existingApps := map[string]struct{}{}
	for _, resource := range requestedResourceIDs {
		switch resource.Kind {
		case types.KindApp:
			existingApps[resource.Name] = struct{}{}
		case types.KindUserGroup:
			userGroups = append(userGroups, resource)
		}
	}

	for _, resource := range userGroups {
		if resource.Kind != types.KindUserGroup {
			continue
		}

		userGroup, err := a.GetUserGroup(ctx, resource.Name)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		for _, app := range userGroup.GetApplications() {
			// Only add to the request if we haven't already added it.
			if _, ok := existingApps[app]; !ok {
				additionalResources = append(additionalResources, types.ResourceID{
					ClusterName: resource.ClusterName,
					Kind:        types.KindApp,
					Name:        app,
				})
				existingApps[app] = struct{}{}
			}
		}
	}

	if len(additionalResources) > 0 {
		requestedResourceIDs = append(requestedResourceIDs, additionalResources...)
		req.SetRequestedResourceIDs(requestedResourceIDs)
	}

	if req.GetDryRun() {
		// Made it this far with no errors, return before creating the request
		// if this is a dry run.
		return req, nil
	}

	if err := a.verifyAccessRequestMonthlyLimit(ctx); err != nil {
		return nil, trace.Wrap(err)
	}

	log.Debugf("Creating Access Request %v with expiry %v.", req.GetName(), req.Expiry())

	if _, err := a.Services.CreateAccessRequestV2(ctx, req); err != nil {
		return nil, trace.Wrap(err)
	}
	err := a.emitter.EmitAuditEvent(a.closeCtx, &apievents.AccessRequestCreate{
		Metadata: apievents.Metadata{
			Type: events.AccessRequestCreateEvent,
			Code: events.AccessRequestCreateCode,
		},
		UserMetadata: authz.ClientUserMetadataWithUser(ctx, req.GetUser()),
		ResourceMetadata: apievents.ResourceMetadata{
			Expires: req.GetAccessExpiry(),
		},
		Roles:                req.GetRoles(),
		RequestedResourceIDs: apievents.ResourceIDs(req.GetRequestedResourceIDs()),
		RequestID:            req.GetName(),
		RequestState:         req.GetState().String(),
		Reason:               req.GetRequestReason(),
		MaxDuration:          req.GetMaxDuration(),
	})
	if err != nil {
		log.WithError(err).Warn("Failed to emit access request create event.")
	}

	accessRequestsCreatedMetric.WithLabelValues(
		strconv.Itoa(len(req.GetRoles())),
		strconv.Itoa(len(req.GetRequestedResourceIDs()))).Inc()
	return req, nil
}

func (a *Server) DeleteAccessRequest(ctx context.Context, name string) error {
	if err := a.Services.DeleteAccessRequest(ctx, name); err != nil {
		return trace.Wrap(err)
	}
	if err := a.emitter.EmitAuditEvent(ctx, &apievents.AccessRequestDelete{
		Metadata: apievents.Metadata{
			Type: events.AccessRequestDeleteEvent,
			Code: events.AccessRequestDeleteCode,
		},
		UserMetadata: authz.ClientUserMetadata(ctx),
		RequestID:    name,
	}); err != nil {
		log.WithError(err).Warn("Failed to emit access request delete event.")
	}
	return nil
}

func (a *Server) SetAccessRequestState(ctx context.Context, params types.AccessRequestUpdate) error {
	req, err := a.Services.SetAccessRequestState(ctx, params)
	if err != nil {
		return trace.Wrap(err)
	}
	event := &apievents.AccessRequestCreate{
		Metadata: apievents.Metadata{
			Type: events.AccessRequestUpdateEvent,
			Code: events.AccessRequestUpdateCode,
		},
		ResourceMetadata: apievents.ResourceMetadata{
			UpdatedBy: authz.ClientUsername(ctx),
			Expires:   req.GetAccessExpiry(),
		},
		RequestID:    params.RequestID,
		RequestState: params.State.String(),
		Reason:       params.Reason,
		Roles:        params.Roles,
	}

	if delegator := apiutils.GetDelegator(ctx); delegator != "" {
		event.Delegator = delegator
	}

	if len(params.Annotations) > 0 {
		annotations, err := apievents.EncodeMapStrings(params.Annotations)
		if err != nil {
			log.WithError(err).Debugf("Failed to encode access request annotations.")
		} else {
			event.Annotations = annotations
		}
	}
	err = a.emitter.EmitAuditEvent(a.closeCtx, event)
	if err != nil {
		log.WithError(err).Warn("Failed to emit access request update event.")
	}
	return trace.Wrap(err)
}

func (a *Server) SubmitAccessReview(ctx context.Context, params types.AccessReviewSubmission) (types.AccessRequest, error) {
	clusterName, err := a.GetClusterName()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// set up a checker for the review author
	checker, err := services.NewReviewPermissionChecker(ctx, a, params.Review.Author)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// don't bother continuing if the author has no allow directives
	if !checker.HasAllowDirectives() {
		return nil, trace.AccessDenied("user %q cannot submit reviews", params.Review.Author)
	}

	// final permission checks and review application must be done by the local backend
	// service, as their validity depends upon optimistic locking.
	req, err := a.ApplyAccessReview(ctx, params, checker)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	event := &apievents.AccessRequestCreate{
		Metadata: apievents.Metadata{
			Type:        events.AccessRequestReviewEvent,
			Code:        events.AccessRequestReviewCode,
			ClusterName: clusterName.GetClusterName(),
		},
		ResourceMetadata: apievents.ResourceMetadata{
			Expires: req.GetAccessExpiry(),
		},
		RequestID:     params.RequestID,
		RequestState:  req.GetState().String(),
		ProposedState: params.Review.ProposedState.String(),
		Reason:        params.Review.Reason,
		Reviewer:      params.Review.Author,
		MaxDuration:   req.GetMaxDuration(),
	}

	if len(params.Review.Annotations) > 0 {
		annotations, err := apievents.EncodeMapStrings(params.Review.Annotations)
		if err != nil {
			log.WithError(err).Debugf("Failed to encode access request annotations.")
		} else {
			event.Annotations = annotations
		}
	}
	if err := a.emitter.EmitAuditEvent(a.closeCtx, event); err != nil {
		log.WithError(err).Warn("Failed to emit access request update event.")
	}

	return req, nil
}

func (a *Server) GetAccessCapabilities(ctx context.Context, req types.AccessCapabilitiesRequest) (*types.AccessCapabilities, error) {
	caps, err := services.CalculateAccessCapabilities(ctx, a.clock, a, req)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return caps, nil
}

// NewKeepAliver returns a new instance of keep aliver
func (a *Server) NewKeepAliver(ctx context.Context) (types.KeepAliver, error) {
	cancelCtx, cancel := context.WithCancel(ctx)
	k := &authKeepAliver{
		a:           a,
		ctx:         cancelCtx,
		cancel:      cancel,
		keepAlivesC: make(chan types.KeepAlive),
	}
	go k.forwardKeepAlives()
	return k, nil
}

// KeepAliveServer implements [services.Presence] by delegating to
// [Server.Services] and potentially emitting a [usagereporter] event.
func (a *Server) KeepAliveServer(ctx context.Context, h types.KeepAlive) error {
	if err := a.Services.KeepAliveServer(ctx, h); err != nil {
		return trace.Wrap(err)
	}

	// ResourceHeartbeatEvent only cares about a few KeepAlive types
	kind := usagereporter.ResourceKindFromKeepAliveType(h.Type)
	if kind == 0 {
		return nil
	}
	a.AnonymizeAndSubmit(&usagereporter.ResourceHeartbeatEvent{
		Name:   h.Name,
		Kind:   kind,
		Static: h.Expires.IsZero(),
	})

	return nil
}

// UpsertNode implements [services.Presence] by delegating to [Server.Services]
// and potentially emitting a [usagereporter] event.
func (a *Server) UpsertNode(ctx context.Context, server types.Server) (*types.KeepAlive, error) {
	lease, err := a.Services.UpsertNode(ctx, server)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	kind := usagereporter.ResourceKindNode
	if server.GetSubKind() == types.SubKindOpenSSHNode {
		kind = usagereporter.ResourceKindNodeOpenSSH
	}

	a.AnonymizeAndSubmit(&usagereporter.ResourceHeartbeatEvent{
		Name:   server.GetName(),
		Kind:   kind,
		Static: server.Expiry().IsZero(),
	})

	return lease, nil
}

// enforceLicense checks if the license allows the given resource type to be
// created.
func enforceLicense(t string) error {
	switch t {
	case types.KindKubeServer, types.KindKubernetesCluster:
		if !modules.GetModules().Features().Kubernetes {
			return trace.AccessDenied(
				"this Teleport cluster is not licensed for Kubernetes, please contact the cluster administrator")
		}
	}
	return nil
}

// UpsertKubernetesServer implements [services.Presence] by delegating to
// [Server.Services] and then potentially emitting a [usagereporter] event.
func (a *Server) UpsertKubernetesServer(ctx context.Context, server types.KubeServer) (*types.KeepAlive, error) {
	if err := enforceLicense(types.KindKubeServer); err != nil {
		return nil, trace.Wrap(err)
	}

	k, err := a.Services.UpsertKubernetesServer(ctx, server)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	a.AnonymizeAndSubmit(&usagereporter.ResourceHeartbeatEvent{
		// the name of types.KubeServer might include a -proxy_service suffix
		Name:   server.GetCluster().GetName(),
		Kind:   usagereporter.ResourceKindKubeServer,
		Static: server.Expiry().IsZero(),
	})

	return k, nil
}

// UpsertApplicationServer implements [services.Presence] by delegating to
// [Server.Services] and then potentially emitting a [usagereporter] event.
func (a *Server) UpsertApplicationServer(ctx context.Context, server types.AppServer) (*types.KeepAlive, error) {
	lease, err := a.Services.UpsertApplicationServer(ctx, server)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	a.AnonymizeAndSubmit(&usagereporter.ResourceHeartbeatEvent{
		Name:   server.GetName(),
		Kind:   usagereporter.ResourceKindAppServer,
		Static: server.Expiry().IsZero(),
	})

	return lease, nil
}

// UpsertDatabaseServer implements [services.Presence] by delegating to
// [Server.Services] and then potentially emitting a [usagereporter] event.
func (a *Server) UpsertDatabaseServer(ctx context.Context, server types.DatabaseServer) (*types.KeepAlive, error) {
	lease, err := a.Services.UpsertDatabaseServer(ctx, server)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	a.AnonymizeAndSubmit(&usagereporter.ResourceHeartbeatEvent{
		Name:   server.GetName(),
		Kind:   usagereporter.ResourceKindDBServer,
		Static: server.Expiry().IsZero(),
	})

	return lease, nil
}

func (a *Server) DeleteWindowsDesktop(ctx context.Context, hostID, name string) error {
	if err := a.Services.DeleteWindowsDesktop(ctx, hostID, name); err != nil {
		return trace.Wrap(err)
	}
	if _, err := a.desktopsLimitExceeded(ctx); err != nil {
		log.Warnf("Can't check OSS non-AD desktops limit: %v", err)
	}
	return nil
}

// CreateWindowsDesktop implements [services.WindowsDesktops] by delegating to
// [Server.Services] and then potentially emitting a [usagereporter] event.
func (a *Server) CreateWindowsDesktop(ctx context.Context, desktop types.WindowsDesktop) error {
	if err := a.Services.CreateWindowsDesktop(ctx, desktop); err != nil {
		return trace.Wrap(err)
	}

	a.AnonymizeAndSubmit(&usagereporter.ResourceHeartbeatEvent{
		Name:   desktop.GetName(),
		Kind:   usagereporter.ResourceKindWindowsDesktop,
		Static: desktop.Expiry().IsZero(),
	})

	return nil
}

// UpdateWindowsDesktop implements [services.WindowsDesktops] by delegating to
// [Server.Services] and then potentially emitting a [usagereporter] event.
func (a *Server) UpdateWindowsDesktop(ctx context.Context, desktop types.WindowsDesktop) error {
	if err := a.Services.UpdateWindowsDesktop(ctx, desktop); err != nil {
		return trace.Wrap(err)
	}

	a.AnonymizeAndSubmit(&usagereporter.ResourceHeartbeatEvent{
		Name:   desktop.GetName(),
		Kind:   usagereporter.ResourceKindWindowsDesktop,
		Static: desktop.Expiry().IsZero(),
	})

	return nil
}

// UpsertWindowsDesktop implements [services.WindowsDesktops] by delegating to
// [Server.Services] and then potentially emitting a [usagereporter] event.
func (a *Server) UpsertWindowsDesktop(ctx context.Context, desktop types.WindowsDesktop) error {
	if err := a.Services.UpsertWindowsDesktop(ctx, desktop); err != nil {
		return trace.Wrap(err)
	}

	a.AnonymizeAndSubmit(&usagereporter.ResourceHeartbeatEvent{
		Name:   desktop.GetName(),
		Kind:   usagereporter.ResourceKindWindowsDesktop,
		Static: desktop.Expiry().IsZero(),
	})

	return nil
}

func (a *Server) streamWindowsDesktops(ctx context.Context, startKey string) stream.Stream[types.WindowsDesktop] {
	var done bool
	return stream.PageFunc(func() ([]types.WindowsDesktop, error) {
		if done {
			return nil, io.EOF
		}
		resp, err := a.ListWindowsDesktops(ctx, types.ListWindowsDesktopsRequest{
			Limit:    50,
			StartKey: startKey,
		})
		if err != nil {
			return nil, trace.Wrap(err)
		}
		startKey = resp.NextKey
		done = startKey == ""
		return resp.Desktops, nil
	})
}

func (a *Server) syncDesktopsLimitAlert(ctx context.Context) {
	exceeded, err := a.desktopsLimitExceeded(ctx)
	if err != nil {
		log.Warnf("Can't check OSS non-AD desktops limit: %v", err)
	}
	if !exceeded {
		return
	}
	alert, err := types.NewClusterAlert(OSSDesktopsAlertID, OSSDesktopsAlertMessage,
		types.WithAlertSeverity(types.AlertSeverity_MEDIUM),
		types.WithAlertLabel(types.AlertOnLogin, "yes"),
		types.WithAlertLabel(types.AlertPermitAll, "yes"),
		types.WithAlertLabel(types.AlertLink, OSSDesktopAlertLink),
		types.WithAlertExpires(time.Now().Add(OSSDesktopsCheckPeriod)))
	if err != nil {
		log.Warnf("Can't create OSS non-AD desktops limit alert: %v", err)
	}
	if err := a.UpsertClusterAlert(ctx, alert); err != nil {
		log.Warnf("Can't upsert OSS non-AD desktops limit alert: %v", err)
	}
}

// desktopsLimitExceeded checks if number of non-AD desktops exceeds limit for OSS distribution. Returns always false for Enterprise.
func (a *Server) desktopsLimitExceeded(ctx context.Context) (bool, error) {
	if modules.GetModules().BuildType() != modules.BuildOSS {
		return false, nil
	}

	desktops := stream.FilterMap(
		a.streamWindowsDesktops(ctx, ""),
		func(d types.WindowsDesktop) (struct{}, bool) {
			return struct{}{}, d.NonAD()
		},
	)
	count := 0
	for desktops.Next() {
		count++
		if count > OSSDesktopsLimit {
			desktops.Done()
			return true, nil
		}
	}
	return false, trace.Wrap(desktops.Done())
}

// GenerateCertAuthorityCRL generates an empty CRL for the local CA of a given type.
func (a *Server) GenerateCertAuthorityCRL(ctx context.Context, caType types.CertAuthType) ([]byte, error) {
	// Generate a CRL for the current cluster CA.
	clusterName, err := a.GetClusterName()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	ca, err := a.GetCertAuthority(ctx, types.CertAuthID{
		Type:       caType,
		DomainName: clusterName.GetClusterName(),
	}, true)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// TODO(awly): this will only create a CRL for an active signer.
	// If there are multiple signers (multiple HSMs), we won't have the full CRL coverage.
	// Generate a CRL per signer and return all of them separately.

	cert, signer, err := a.keyStore.GetTLSCertAndSigner(ctx, ca)
	if trace.IsNotFound(err) {
		// If there is no local TLS signer found in the host CA ActiveKeys, this
		// auth server may have a newly configured HSM and has only populated
		// local keys in the AdditionalTrustedKeys until the next CA rotation.
		// This is the only case where we should be able to get a signer from
		// AdditionalTrustedKeys but not ActiveKeys.
		cert, signer, err = a.keyStore.GetAdditionalTrustedTLSCertAndSigner(ctx, ca)
	}
	if err != nil {
		return nil, trace.Wrap(err)
	}
	tlsAuthority, err := tlsca.FromCertAndSigner(cert, signer)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	// Empty CRL valid for 1yr.
	template := &x509.RevocationList{
		Number:     big.NewInt(1),
		ThisUpdate: time.Now().Add(-1 * time.Minute), // 1 min in the past to account for clock skew.
		NextUpdate: time.Now().Add(365 * 24 * time.Hour),
	}
	crl, err := x509.CreateRevocationList(rand.Reader, template, tlsAuthority.Cert, tlsAuthority.Signer)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return crl, nil
}

// ErrDone indicates that resource iteration is complete
var ErrDone = errors.New("done iterating")

// IterateResources loads all resources matching the provided request and passes them one by one to the provided
// callback function. To stop iteration callers may return ErrDone from the callback function, which will result in
// a nil return from IterateResources. Any other errors returned from the callback function cause iteration to stop
// and the error to be returned.
func (a *Server) IterateResources(ctx context.Context, req proto.ListResourcesRequest, f func(resource types.ResourceWithLabels) error) error {
	for {
		resp, err := a.ListResources(ctx, req)
		if err != nil {
			return trace.Wrap(err)
		}

		for _, resource := range resp.Resources {
			if err := f(resource); err != nil {
				if errors.Is(err, ErrDone) {
					return nil
				}
				return trace.Wrap(err)
			}
		}

		if resp.NextKey == "" {
			return nil
		}

		req.StartKey = resp.NextKey
	}
}

// CreateApp creates a new application resource.
func (a *Server) CreateApp(ctx context.Context, app types.Application) error {
	if err := a.Services.CreateApp(ctx, app); err != nil {
		return trace.Wrap(err)
	}
	if err := a.emitter.EmitAuditEvent(ctx, &apievents.AppCreate{
		Metadata: apievents.Metadata{
			Type: events.AppCreateEvent,
			Code: events.AppCreateCode,
		},
		UserMetadata: authz.ClientUserMetadata(ctx),
		ResourceMetadata: apievents.ResourceMetadata{
			Name:    app.GetName(),
			Expires: app.Expiry(),
		},
		AppMetadata: apievents.AppMetadata{
			AppURI:        app.GetURI(),
			AppPublicAddr: app.GetPublicAddr(),
			AppLabels:     app.GetStaticLabels(),
		},
	}); err != nil {
		log.WithError(err).Warn("Failed to emit app create event.")
	}
	return nil
}

// UpdateApp updates an existing application resource.
func (a *Server) UpdateApp(ctx context.Context, app types.Application) error {
	if err := a.Services.UpdateApp(ctx, app); err != nil {
		return trace.Wrap(err)
	}
	if err := a.emitter.EmitAuditEvent(ctx, &apievents.AppUpdate{
		Metadata: apievents.Metadata{
			Type: events.AppUpdateEvent,
			Code: events.AppUpdateCode,
		},
		UserMetadata: authz.ClientUserMetadata(ctx),
		ResourceMetadata: apievents.ResourceMetadata{
			Name:    app.GetName(),
			Expires: app.Expiry(),
		},
		AppMetadata: apievents.AppMetadata{
			AppURI:        app.GetURI(),
			AppPublicAddr: app.GetPublicAddr(),
			AppLabels:     app.GetStaticLabels(),
		},
	}); err != nil {
		log.WithError(err).Warn("Failed to emit app update event.")
	}
	return nil
}

// DeleteApp deletes an application resource.
func (a *Server) DeleteApp(ctx context.Context, name string) error {
	if err := a.Services.DeleteApp(ctx, name); err != nil {
		return trace.Wrap(err)
	}
	if err := a.emitter.EmitAuditEvent(ctx, &apievents.AppDelete{
		Metadata: apievents.Metadata{
			Type: events.AppDeleteEvent,
			Code: events.AppDeleteCode,
		},
		UserMetadata: authz.ClientUserMetadata(ctx),
		ResourceMetadata: apievents.ResourceMetadata{
			Name: name,
		},
	}); err != nil {
		log.WithError(err).Warn("Failed to emit app delete event.")
	}
	return nil
}

// CreateSessionTracker creates a tracker resource for an active session.
func (a *Server) CreateSessionTracker(ctx context.Context, tracker types.SessionTracker) (types.SessionTracker, error) {
	// Don't allow sessions that require moderation without the enterprise feature enabled.
	for _, policySet := range tracker.GetHostPolicySets() {
		if len(policySet.RequireSessionJoin) != 0 {
			if modules.GetModules().BuildType() != modules.BuildEnterprise {
				return nil, fmt.Errorf("Moderated Sessions: %w", ErrRequiresEnterprise)
			}
		}
	}

	return a.Services.CreateSessionTracker(ctx, tracker)
}

// CreateDatabase creates a new database resource.
func (a *Server) CreateDatabase(ctx context.Context, database types.Database) error {
	if err := a.Services.CreateDatabase(ctx, database); err != nil {
		return trace.Wrap(err)
	}
	if err := a.emitter.EmitAuditEvent(ctx, &apievents.DatabaseCreate{
		Metadata: apievents.Metadata{
			Type: events.DatabaseCreateEvent,
			Code: events.DatabaseCreateCode,
		},
		UserMetadata: authz.ClientUserMetadata(ctx),
		ResourceMetadata: apievents.ResourceMetadata{
			Name:    database.GetName(),
			Expires: database.Expiry(),
		},
		DatabaseMetadata: apievents.DatabaseMetadata{
			DatabaseProtocol:             database.GetProtocol(),
			DatabaseURI:                  database.GetURI(),
			DatabaseLabels:               database.GetStaticLabels(),
			DatabaseAWSRegion:            database.GetAWS().Region,
			DatabaseAWSRedshiftClusterID: database.GetAWS().Redshift.ClusterID,
			DatabaseGCPProjectID:         database.GetGCP().ProjectID,
			DatabaseGCPInstanceID:        database.GetGCP().InstanceID,
		},
	}); err != nil {
		log.WithError(err).Warn("Failed to emit database create event.")
	}
	return nil
}

// UpdateDatabase updates an existing database resource.
func (a *Server) UpdateDatabase(ctx context.Context, database types.Database) error {
	if err := a.Services.UpdateDatabase(ctx, database); err != nil {
		return trace.Wrap(err)
	}
	if err := a.emitter.EmitAuditEvent(ctx, &apievents.DatabaseUpdate{
		Metadata: apievents.Metadata{
			Type: events.DatabaseUpdateEvent,
			Code: events.DatabaseUpdateCode,
		},
		UserMetadata: authz.ClientUserMetadata(ctx),
		ResourceMetadata: apievents.ResourceMetadata{
			Name:    database.GetName(),
			Expires: database.Expiry(),
		},
		DatabaseMetadata: apievents.DatabaseMetadata{
			DatabaseProtocol:             database.GetProtocol(),
			DatabaseURI:                  database.GetURI(),
			DatabaseLabels:               database.GetStaticLabels(),
			DatabaseAWSRegion:            database.GetAWS().Region,
			DatabaseAWSRedshiftClusterID: database.GetAWS().Redshift.ClusterID,
			DatabaseGCPProjectID:         database.GetGCP().ProjectID,
			DatabaseGCPInstanceID:        database.GetGCP().InstanceID,
		},
	}); err != nil {
		log.WithError(err).Warn("Failed to emit database update event.")
	}
	return nil
}

// DeleteDatabase deletes a database resource.
func (a *Server) DeleteDatabase(ctx context.Context, name string) error {
	if err := a.Services.DeleteDatabase(ctx, name); err != nil {
		return trace.Wrap(err)
	}
	if err := a.emitter.EmitAuditEvent(ctx, &apievents.DatabaseDelete{
		Metadata: apievents.Metadata{
			Type: events.DatabaseDeleteEvent,
			Code: events.DatabaseDeleteCode,
		},
		UserMetadata: authz.ClientUserMetadata(ctx),
		ResourceMetadata: apievents.ResourceMetadata{
			Name: name,
		},
	}); err != nil {
		log.WithError(err).Warn("Failed to emit database delete event.")
	}
	return nil
}

// ListResources returns paginated resources depending on the resource type..
func (a *Server) ListResources(ctx context.Context, req proto.ListResourcesRequest) (*types.ListResourcesResponse, error) {
	// Because WindowsDesktopService does not contain the desktop resources,
	// this is not implemented at the cache level and requires the workaround
	// here in order to support KindWindowsDesktop for ListResources.
	if req.ResourceType == types.KindWindowsDesktop {
		wResp, err := a.ListWindowsDesktops(ctx, types.ListWindowsDesktopsRequest{
			WindowsDesktopFilter: req.WindowsDesktopFilter,
			Limit:                int(req.Limit),
			StartKey:             req.StartKey,
			PredicateExpression:  req.PredicateExpression,
			Labels:               req.Labels,
			SearchKeywords:       req.SearchKeywords,
		})
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return &types.ListResourcesResponse{
			Resources: types.WindowsDesktops(wResp.Desktops).AsResources(),
			NextKey:   wResp.NextKey,
		}, nil
	}
	if req.ResourceType == types.KindWindowsDesktopService {
		wResp, err := a.ListWindowsDesktopServices(ctx, types.ListWindowsDesktopServicesRequest{
			Limit:               int(req.Limit),
			StartKey:            req.StartKey,
			PredicateExpression: req.PredicateExpression,
			Labels:              req.Labels,
			SearchKeywords:      req.SearchKeywords,
		})
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return &types.ListResourcesResponse{
			Resources: types.WindowsDesktopServices(wResp.DesktopServices).AsResources(),
			NextKey:   wResp.NextKey,
		}, nil
	}
	return a.Cache.ListResources(ctx, req)
}

// CreateKubernetesCluster creates a new kubernetes cluster resource.
func (a *Server) CreateKubernetesCluster(ctx context.Context, kubeCluster types.KubeCluster) error {
	if err := enforceLicense(types.KindKubernetesCluster); err != nil {
		return trace.Wrap(err)
	}
	if err := a.Services.CreateKubernetesCluster(ctx, kubeCluster); err != nil {
		return trace.Wrap(err)
	}
	if err := a.emitter.EmitAuditEvent(ctx, &apievents.KubernetesClusterCreate{
		Metadata: apievents.Metadata{
			Type: events.KubernetesClusterCreateEvent,
			Code: events.KubernetesClusterCreateCode,
		},
		UserMetadata: authz.ClientUserMetadata(ctx),
		ResourceMetadata: apievents.ResourceMetadata{
			Name:    kubeCluster.GetName(),
			Expires: kubeCluster.Expiry(),
		},
		KubeClusterMetadata: apievents.KubeClusterMetadata{
			KubeLabels: kubeCluster.GetStaticLabels(),
		},
	}); err != nil {
		log.WithError(err).Warn("Failed to emit kube cluster create event.")
	}
	return nil
}

// UpdateKubernetesCluster updates an existing kubernetes cluster resource.
func (a *Server) UpdateKubernetesCluster(ctx context.Context, kubeCluster types.KubeCluster) error {
	if err := enforceLicense(types.KindKubernetesCluster); err != nil {
		return trace.Wrap(err)
	}
	if err := a.Kubernetes.UpdateKubernetesCluster(ctx, kubeCluster); err != nil {
		return trace.Wrap(err)
	}
	if err := a.emitter.EmitAuditEvent(ctx, &apievents.KubernetesClusterUpdate{
		Metadata: apievents.Metadata{
			Type: events.KubernetesClusterUpdateEvent,
			Code: events.KubernetesClusterUpdateCode,
		},
		UserMetadata: authz.ClientUserMetadata(ctx),
		ResourceMetadata: apievents.ResourceMetadata{
			Name:    kubeCluster.GetName(),
			Expires: kubeCluster.Expiry(),
		},
		KubeClusterMetadata: apievents.KubeClusterMetadata{
			KubeLabels: kubeCluster.GetStaticLabels(),
		},
	}); err != nil {
		log.WithError(err).Warn("Failed to emit kube cluster update event.")
	}
	return nil
}

// DeleteKubernetesCluster deletes a kubernetes cluster resource.
func (a *Server) DeleteKubernetesCluster(ctx context.Context, name string) error {
	if err := a.Kubernetes.DeleteKubernetesCluster(ctx, name); err != nil {
		return trace.Wrap(err)
	}
	if err := a.emitter.EmitAuditEvent(ctx, &apievents.KubernetesClusterDelete{
		Metadata: apievents.Metadata{
			Type: events.KubernetesClusterDeleteEvent,
			Code: events.KubernetesClusterDeleteCode,
		},
		UserMetadata: authz.ClientUserMetadata(ctx),
		ResourceMetadata: apievents.ResourceMetadata{
			Name: name,
		},
	}); err != nil {
		log.WithError(err).Warn("Failed to emit kube cluster delete event.")
	}
	return nil
}

// SubmitUsageEvent submits an external usage event.
func (a *Server) SubmitUsageEvent(ctx context.Context, req *proto.SubmitUsageEventRequest) error {
	username, err := authz.GetClientUsername(ctx)
	if err != nil {
		return trace.Wrap(err)
	}

	userIsSSO, err := authz.GetClientUserIsSSO(ctx)
	if err != nil {
		return trace.Wrap(err)
	}

	userMetadata := usagereporter.UserMetadata{
		Username: username,
		IsSSO:    userIsSSO,
	}

	event, err := usagereporter.ConvertUsageEvent(req.GetEvent(), userMetadata)
	if err != nil {
		return trace.Wrap(err)
	}

	a.AnonymizeAndSubmit(event)

	return nil
}

// Ping gets basic info about the auth server.
// Please note that Ping is publicly accessible (not protected by any RBAC) by design,
// and thus PingResponse must never contain any sensitive information.
func (a *Server) Ping(ctx context.Context) (proto.PingResponse, error) {
	cn, err := a.GetClusterName()
	if err != nil {
		return proto.PingResponse{}, trace.Wrap(err)
	}

	return proto.PingResponse{
		ClusterName:     cn.GetClusterName(),
		ServerVersion:   teleport.Version,
		ServerFeatures:  modules.GetModules().Features().ToProto(),
		ProxyPublicAddr: a.getProxyPublicAddr(),
		IsBoring:        modules.GetModules().IsBoringBinary(),
		LoadAllCAs:      a.loadAllCAs,
	}, nil
}

type maintenanceWindowCacheKey struct {
	key string
}

// agentWindowLookahead is the number of upgrade windows, starting from 'today', that we export
// when compiling agent upgrade schedules. The choice is arbitrary. We must export at least 2, because upgraders
// treat a schedule value whose windows all end in the past to be stale and therefore a sign that the agent is
// unhealthy. 3 was picked to give us some leeway in terms of how long an agent can be turned off before its
// upgrader starts complaining of a stale schedule.
const agentWindowLookahead = 3

// exportUpgradeWindowsCached generates the export value of all upgrade window schedule types. Since schedules
// are reloaded frequently in large clusters and export incurs string/json encoding, we use the ttl cache to store
// the encoded schedule values for a few seconds.
func (a *Server) exportUpgradeWindowsCached(ctx context.Context) (proto.ExportUpgradeWindowsResponse, error) {
	return utils.FnCacheGet(ctx, a.ttlCache, maintenanceWindowCacheKey{"export"}, func(ctx context.Context) (proto.ExportUpgradeWindowsResponse, error) {
		var rsp proto.ExportUpgradeWindowsResponse
		cmc, err := a.GetClusterMaintenanceConfig(ctx)
		if err != nil {
			if trace.IsNotFound(err) {
				// "not found" is treated as an empty schedule value
				return rsp, nil
			}
			return rsp, trace.Wrap(err)
		}

		agentWindow, ok := cmc.GetAgentUpgradeWindow()
		if !ok {
			// "unconfigured" is treated as an empty schedule value
			return rsp, nil
		}

		sched := agentWindow.Export(time.Now(), agentWindowLookahead)

		rsp.CanonicalSchedule = &sched

		rsp.KubeControllerSchedule, err = uw.EncodeKubeControllerSchedule(sched)
		if err != nil {
			log.Warnf("Failed to encode kube controller maintenance schedule: %v", err)
		}

		rsp.SystemdUnitSchedule, err = uw.EncodeSystemdUnitSchedule(sched)
		if err != nil {
			log.Warnf("Failed to encode systemd unit maintenance schedule: %v", err)
		}

		return rsp, nil
	})
}

func (a *Server) ExportUpgradeWindows(ctx context.Context, req proto.ExportUpgradeWindowsRequest) (proto.ExportUpgradeWindowsResponse, error) {
	var rsp proto.ExportUpgradeWindowsResponse

	// get the cached collection of all export values
	cached, err := a.exportUpgradeWindowsCached(ctx)
	if err != nil {
		return rsp, nil
	}

	switch req.UpgraderKind {
	case "":
		rsp.CanonicalSchedule = cached.CanonicalSchedule.Clone()
	case types.UpgraderKindKubeController:
		rsp.KubeControllerSchedule = cached.KubeControllerSchedule

		if sched := os.Getenv("TELEPORT_UNSTABLE_KUBE_UPGRADE_SCHEDULE"); sched != "" {
			rsp.KubeControllerSchedule = sched
		}
	case types.UpgraderKindSystemdUnit:
		rsp.SystemdUnitSchedule = cached.SystemdUnitSchedule

		if sched := os.Getenv("TELEPORT_UNSTABLE_SYSTEMD_UPGRADE_SCHEDULE"); sched != "" {
			rsp.SystemdUnitSchedule = sched
		}
	default:
		return rsp, trace.NotImplemented("unsupported upgrader kind %q in upgrade window export request", req.UpgraderKind)
	}

	return rsp, nil
}

func (a *Server) isMFARequired(ctx context.Context, checker services.AccessChecker, req *proto.IsMFARequiredRequest) (*proto.IsMFARequiredResponse, error) {
	authPref, err := a.GetAuthPreference(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	switch state := checker.GetAccessState(authPref); state.MFARequired {
	case services.MFARequiredAlways:
		return &proto.IsMFARequiredResponse{Required: true}, nil
	case services.MFARequiredNever:
		return &proto.IsMFARequiredResponse{Required: false}, nil
	}

	var noMFAAccessErr, notFoundErr error
	switch t := req.Target.(type) {
	case *proto.IsMFARequiredRequest_Node:
		if t.Node.Node == "" {
			return nil, trace.BadParameter("empty Node field")
		}
		if t.Node.Login == "" {
			return nil, trace.BadParameter("empty Login field")
		}

		// Find the target node and check whether MFA is required.
		matches, err := client.GetResourcesWithFilters(ctx, a, proto.ListResourcesRequest{
			ResourceType:   types.KindNode,
			Namespace:      apidefaults.Namespace,
			SearchKeywords: []string{t.Node.Node},
		})
		if err != nil {
			return nil, trace.Wrap(err)
		}

		if len(matches) == 0 {
			// If t.Node.Node is not a known registered node, it may be an
			// unregistered host running OpenSSH with a certificate created via
			// `tctl auth sign`. In these cases, let the user through without
			// extra checks.
			//
			// If t.Node.Node turns out to be an alias for a real node (e.g.
			// private network IP), and MFA check was actually required, the
			// Node itself will check the cert extensions and reject the
			// connection.
			return &proto.IsMFARequiredResponse{Required: false}, nil
		}

		// Check RBAC against all matching nodes and return the first error.
		// If at least one node requires MFA, we'll catch it.
		for _, n := range matches {
			srv, ok := n.(types.Server)
			if !ok {
				continue
			}

			// Filter out any matches on labels before checking access
			fieldVals := append(srv.GetPublicAddrs(), srv.GetName(), srv.GetHostname(), srv.GetAddr())
			if !types.MatchSearch(fieldVals, []string{t.Node.Node}, nil) {
				continue
			}

			err = checker.CheckAccess(
				n,
				services.AccessState{},
				services.NewLoginMatcher(t.Node.Login),
			)

			// Ignore other errors; they'll be caught on the real access attempt.
			if err != nil && errors.Is(err, services.ErrSessionMFARequired) {
				noMFAAccessErr = err
				break
			}
		}

	case *proto.IsMFARequiredRequest_KubernetesCluster:
		notFoundErr = trace.NotFound("kubernetes cluster %q not found", t.KubernetesCluster)
		if t.KubernetesCluster == "" {
			return nil, trace.BadParameter("missing KubernetesCluster field in a kubernetes-only UserCertsRequest")
		}
		// Find the target cluster and check whether MFA is required.
		svcs, err := a.GetKubernetesServers(ctx)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		var cluster types.KubeCluster
		for _, svc := range svcs {
			kubeCluster := svc.GetCluster()
			if kubeCluster.GetName() == t.KubernetesCluster {
				cluster = kubeCluster
				break
			}
		}
		if cluster == nil {
			return nil, trace.Wrap(notFoundErr)
		}

		noMFAAccessErr = checker.CheckAccess(cluster, services.AccessState{})

	case *proto.IsMFARequiredRequest_Database:
		notFoundErr = trace.NotFound("database service %q not found", t.Database.ServiceName)
		if t.Database.ServiceName == "" {
			return nil, trace.BadParameter("missing ServiceName field in a database-only UserCertsRequest")
		}
		servers, err := a.GetDatabaseServers(ctx, apidefaults.Namespace)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		var db types.Database
		for _, server := range servers {
			if server.GetDatabase().GetName() == t.Database.ServiceName {
				db = server.GetDatabase()
				break
			}
		}
		if db == nil {
			return nil, trace.Wrap(notFoundErr)
		}

		autoCreate, _, err := checker.CheckDatabaseRoles(db)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		dbRoleMatchers := role.GetDatabaseRoleMatchers(role.RoleMatchersConfig{
			Database:       db,
			DatabaseUser:   t.Database.Username,
			DatabaseName:   t.Database.GetDatabase(),
			AutoCreateUser: autoCreate,
		})
		noMFAAccessErr = checker.CheckAccess(
			db,
			services.AccessState{},
			dbRoleMatchers...,
		)
	case *proto.IsMFARequiredRequest_WindowsDesktop:
		desktops, err := a.GetWindowsDesktops(ctx, types.WindowsDesktopFilter{Name: t.WindowsDesktop.GetWindowsDesktop()})
		if err != nil {
			return nil, trace.Wrap(err)
		}
		if len(desktops) == 0 {
			return nil, trace.NotFound("windows desktop %q not found", t.WindowsDesktop.GetWindowsDesktop())
		}

		noMFAAccessErr = checker.CheckAccess(desktops[0],
			services.AccessState{},
			services.NewWindowsLoginMatcher(t.WindowsDesktop.GetLogin()))

	default:
		return nil, trace.BadParameter("unknown Target %T", req.Target)
	}
	// No error means that MFA is not required for this resource by
	// AccessChecker.
	if noMFAAccessErr == nil {
		return &proto.IsMFARequiredResponse{Required: false}, nil
	}
	// Errors other than ErrSessionMFARequired mean something else is wrong,
	// most likely access denied.
	if !errors.Is(noMFAAccessErr, services.ErrSessionMFARequired) {
		if !trace.IsAccessDenied(noMFAAccessErr) {
			log.WithError(noMFAAccessErr).Warn("Could not determine MFA access")
		}

		// Mask the access denied errors by returning false to prevent resource
		// name oracles. Auth will be denied (and generate an audit log entry)
		// when the client attempts to connect.
		return &proto.IsMFARequiredResponse{Required: false}, nil
	}
	// If we reach here, the error from AccessChecker was
	// ErrSessionMFARequired.

	return &proto.IsMFARequiredResponse{Required: true}, nil
}

// mfaAuthChallenge constructs an MFAAuthenticateChallenge for all MFA devices
// registered by the user.
func (a *Server) mfaAuthChallenge(ctx context.Context, user string, passwordless bool) (*proto.MFAAuthenticateChallenge, error) {
	// Check what kind of MFA is enabled.
	apref, err := a.GetAuthPreference(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	enableTOTP := apref.IsSecondFactorTOTPAllowed()
	enableWebauthn := apref.IsSecondFactorWebauthnAllowed()

	// Fetch configurations. The IsSecondFactor*Allowed calls above already
	// include the necessary checks of config empty, disabled, etc.
	var u2fPref *types.U2F
	switch val, err := apref.GetU2F(); {
	case trace.IsNotFound(err): // OK, may happen.
	case err != nil: // NOK, unexpected.
		return nil, trace.Wrap(err)
	default:
		u2fPref = val
	}
	var webConfig *types.Webauthn
	switch val, err := apref.GetWebauthn(); {
	case trace.IsNotFound(err): // OK, may happen.
	case err != nil: // NOK, unexpected.
		return nil, trace.Wrap(err)
	default:
		webConfig = val
	}

	// Handle passwordless separately, it works differently from MFA.
	if passwordless {
		if !enableWebauthn {
			return nil, trace.BadParameter("passwordless requires WebAuthn")
		}
		webLogin := &wanlib.PasswordlessFlow{
			Webauthn: webConfig,
			Identity: a.Services,
		}
		assertion, err := webLogin.Begin(ctx)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return &proto.MFAAuthenticateChallenge{
			WebauthnChallenge: wantypes.CredentialAssertionToProto(assertion),
		}, nil
	}

	// User required for non-passwordless.
	if user == "" {
		return nil, trace.BadParameter("user required")
	}

	devs, err := a.Services.GetMFADevices(ctx, user, true /* withSecrets */)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	groupedDevs := groupByDeviceType(devs, enableWebauthn)
	challenge := &proto.MFAAuthenticateChallenge{}

	// TOTP challenge.
	if enableTOTP && groupedDevs.TOTP {
		challenge.TOTP = &proto.TOTPChallenge{}
	}

	// WebAuthn challenge.
	if len(groupedDevs.Webauthn) > 0 {
		webLogin := &wanlib.LoginFlow{
			U2F:      u2fPref,
			Webauthn: webConfig,
			Identity: wanlib.WithDevices(a.Services, groupedDevs.Webauthn),
		}
		assertion, err := webLogin.Begin(ctx, user)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		challenge.WebauthnChallenge = wantypes.CredentialAssertionToProto(assertion)
	}

	return challenge, nil
}

type devicesByType struct {
	TOTP     bool
	Webauthn []*types.MFADevice
}

func groupByDeviceType(devs []*types.MFADevice, groupWebauthn bool) devicesByType {
	res := devicesByType{}
	for _, dev := range devs {
		switch dev.Device.(type) {
		case *types.MFADevice_Totp:
			res.TOTP = true
		case *types.MFADevice_U2F:
			if groupWebauthn {
				res.Webauthn = append(res.Webauthn, dev)
			}
		case *types.MFADevice_Webauthn:
			if groupWebauthn {
				res.Webauthn = append(res.Webauthn, dev)
			}
		default:
			log.Warningf("Skipping MFA device of unknown type %T.", dev.Device)
		}
	}
	return res
}

// validateMFAAuthResponse validates an MFA or passwordless challenge.
// Returns the device used to solve the challenge (if applicable) and the
// username.
func (a *Server) validateMFAAuthResponse(
	ctx context.Context,
	resp *proto.MFAAuthenticateResponse, user string, passwordless bool,
) (*types.MFADevice, string, error) {
	// Sanity check user/passwordless.
	if user == "" && !passwordless {
		return nil, "", trace.BadParameter("user required")
	}

	switch res := resp.Response.(type) {
	// cases in order of preference
	case *proto.MFAAuthenticateResponse_Webauthn:
		// Read necessary configurations.
		cap, err := a.GetAuthPreference(ctx)
		if err != nil {
			return nil, "", trace.Wrap(err)
		}
		u2f, err := cap.GetU2F()
		switch {
		case trace.IsNotFound(err): // OK, may happen.
		case err != nil: // Unexpected.
			return nil, "", trace.Wrap(err)
		}
		webConfig, err := cap.GetWebauthn()
		if err != nil {
			return nil, "", trace.Wrap(err)
		}

		assertionResp := wantypes.CredentialAssertionResponseFromProto(res.Webauthn)
		var dev *types.MFADevice
		if passwordless {
			webLogin := &wanlib.PasswordlessFlow{
				Webauthn: webConfig,
				Identity: a.Services,
			}
			dev, user, err = webLogin.Finish(ctx, assertionResp)
		} else {
			webLogin := &wanlib.LoginFlow{
				U2F:      u2f,
				Webauthn: webConfig,
				Identity: a.Services,
			}
			dev, err = webLogin.Finish(ctx, user, wantypes.CredentialAssertionResponseFromProto(res.Webauthn))
		}
		if err != nil {
			return nil, "", trace.AccessDenied("MFA response validation failed: %v", err)
		}
		return dev, user, nil

	case *proto.MFAAuthenticateResponse_TOTP:
		dev, err := a.checkOTP(user, res.TOTP.Code)
		return dev, user, trace.Wrap(err)

	default:
		return nil, "", trace.BadParameter("unknown or missing MFAAuthenticateResponse type %T", resp.Response)
	}
}

func (a *Server) upsertWebSession(ctx context.Context, user string, session types.WebSession) error {
	if err := a.WebSessions().Upsert(ctx, session); err != nil {
		return trace.Wrap(err)
	}
	token, err := types.NewWebToken(session.GetBearerTokenExpiryTime(), types.WebTokenSpecV3{
		User:  session.GetUser(),
		Token: session.GetBearerToken(),
	})
	if err != nil {
		return trace.Wrap(err)
	}
	if err := a.WebTokens().Upsert(ctx, token); err != nil {
		return trace.Wrap(err)
	}
	return nil
}

func mergeKeySets(a, b types.CAKeySet) types.CAKeySet {
	newKeySet := a.Clone()
	newKeySet.SSH = append(newKeySet.SSH, b.SSH...)
	newKeySet.TLS = append(newKeySet.TLS, b.TLS...)
	newKeySet.JWT = append(newKeySet.JWT, b.JWT...)
	return newKeySet
}

// addAdditionalTrustedKeysAtomic performs an atomic CompareAndSwap to update
// the given CA with newKeys added to the AdditionalTrustedKeys
func (a *Server) addAdditionalTrustedKeysAtomic(
	ctx context.Context,
	currentCA types.CertAuthority,
	newKeys types.CAKeySet,
	needsUpdate func(types.CertAuthority) (bool, error),
) error {
	for {
		select {
		case <-ctx.Done():
			return trace.Wrap(ctx.Err())
		default:
		}
		updateRequired, err := needsUpdate(currentCA)
		if err != nil {
			return trace.Wrap(err)
		}
		if !updateRequired {
			return nil
		}

		newCA := currentCA.Clone()
		currentKeySet := newCA.GetAdditionalTrustedKeys()
		mergedKeySet := mergeKeySets(currentKeySet, newKeys)
		if err := newCA.SetAdditionalTrustedKeys(mergedKeySet); err != nil {
			return trace.Wrap(err)
		}

		err = a.CompareAndSwapCertAuthority(newCA, currentCA)
		if err != nil && !trace.IsCompareFailed(err) {
			return trace.Wrap(err)
		}
		if err == nil {
			// success!
			return nil
		}
		// else trace.IsCompareFailed(err) == true (CA was concurrently updated)

		currentCA, err = a.Services.GetCertAuthority(ctx, currentCA.GetID(), true)
		if err != nil {
			return trace.Wrap(err)
		}
	}
}

// newKeySet generates a new sets of keys for a given CA type.
// Keep this function in sync with lib/service/suite/suite.go:NewTestCAWithConfig().
func newKeySet(ctx context.Context, keyStore *keystore.Manager, caID types.CertAuthID) (types.CAKeySet, error) {
	var keySet types.CAKeySet
	switch caID.Type {
	case types.UserCA, types.HostCA:
		sshKeyPair, err := keyStore.NewSSHKeyPair(ctx)
		if err != nil {
			return keySet, trace.Wrap(err)
		}
		tlsKeyPair, err := keyStore.NewTLSKeyPair(ctx, caID.DomainName)
		if err != nil {
			return keySet, trace.Wrap(err)
		}
		keySet.SSH = append(keySet.SSH, sshKeyPair)
		keySet.TLS = append(keySet.TLS, tlsKeyPair)
	case types.DatabaseCA:
		// Database CA only contains TLS cert.
		tlsKeyPair, err := keyStore.NewTLSKeyPair(ctx, caID.DomainName)
		if err != nil {
			return keySet, trace.Wrap(err)
		}
		keySet.TLS = append(keySet.TLS, tlsKeyPair)
	case types.OpenSSHCA:
		// OpenSSH CA only contains a SSH key pair.
		sshKeyPair, err := keyStore.NewSSHKeyPair(ctx)
		if err != nil {
			return keySet, trace.Wrap(err)
		}
		keySet.SSH = append(keySet.SSH, sshKeyPair)
	case types.JWTSigner, types.OIDCIdPCA:
		jwtKeyPair, err := keyStore.NewJWTKeyPair(ctx)
		if err != nil {
			return keySet, trace.Wrap(err)
		}
		keySet.JWT = append(keySet.JWT, jwtKeyPair)
	case types.SAMLIDPCA:
		// SAML IDP CA only contains TLS certs.
		tlsKeyPair, err := keyStore.NewTLSKeyPair(ctx, caID.DomainName)
		if err != nil {
			return keySet, trace.Wrap(err)
		}
		keySet.TLS = append(keySet.TLS, tlsKeyPair)
	default:
		return keySet, trace.BadParameter("unknown ca type: %s", caID.Type)
	}
	return keySet, nil
}

// ensureLocalAdditionalKeys adds additional trusted keys to the CA if they are not
// already present.
func (a *Server) ensureLocalAdditionalKeys(ctx context.Context, ca types.CertAuthority) error {
	hasUsableKeys, err := a.keyStore.HasUsableAdditionalKeys(ctx, ca)
	if err != nil {
		return trace.Wrap(err)
	}
	if hasUsableKeys {
		// nothing to do
		return nil
	}

	newKeySet, err := newKeySet(ctx, a.keyStore, ca.GetID())
	if err != nil {
		return trace.Wrap(err)
	}

	// The CA still needs an update while the keystore does not have any usable
	// keys in the CA.
	needsUpdate := func(ca types.CertAuthority) (bool, error) {
		hasUsableKeys, err := a.keyStore.HasUsableAdditionalKeys(ctx, ca)
		return !hasUsableKeys, trace.Wrap(err)
	}
	err = a.addAdditionalTrustedKeysAtomic(ctx, ca, newKeySet, needsUpdate)
	if err != nil {
		return trace.Wrap(err)
	}
	log.Infof("Successfully added locally usable additional trusted keys to %s CA.", ca.GetType())
	return nil
}

// GetLicense return the license used the start the teleport enterprise auth server
func (a *Server) GetLicense(ctx context.Context) (string, error) {
	if modules.GetModules().Features().Cloud {
		return "", trace.AccessDenied("license cannot be downloaded on Cloud")
	}
	if a.license == nil {
		return "", trace.NotFound("license not found")
	}
	return fmt.Sprintf("%s%s", a.license.CertPEM, a.license.KeyPEM), nil
}

// GetHeadlessAuthenticationFromWatcher gets a headless authentication from the headless
// authentication watcher.
func (a *Server) GetHeadlessAuthenticationFromWatcher(ctx context.Context, username, name string) (*types.HeadlessAuthentication, error) {
	sub, err := a.headlessAuthenticationWatcher.Subscribe(ctx, username, name)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer sub.Close()

	// Wait for the login process to insert the headless authentication resource into the backend.
	// If it already exists and passes the condition, WaitForUpdate will return it immediately.
	headlessAuthn, err := sub.WaitForUpdate(ctx, func(ha *types.HeadlessAuthentication) (bool, error) {
		return services.ValidateHeadlessAuthentication(ha) == nil, nil
	})
	return headlessAuthn, trace.Wrap(err)
}

// UpsertHeadlessAuthenticationStub creates a headless authentication stub for the user
// that will expire after the standard callback timeout.
func (a *Server) UpsertHeadlessAuthenticationStub(ctx context.Context, username string) error {
	// Create the stub. If it already exists, update its expiration.
	expires := a.clock.Now().Add(defaults.CallbackTimeout)
	stub, err := types.NewHeadlessAuthentication(username, services.HeadlessAuthenticationUserStubID, expires)
	if err != nil {
		return trace.Wrap(err)
	}

	err = a.Services.UpsertHeadlessAuthentication(ctx, stub)
	return trace.Wrap(err)
}

// GetAssistantMessages returns all messages with given conversation ID.
func (a *Server) GetAssistantMessages(ctx context.Context, req *assist.GetAssistantMessagesRequest) (*assist.GetAssistantMessagesResponse, error) {
	resp, err := a.Services.GetAssistantMessages(ctx, req)
	return resp, trace.Wrap(err)
}

// CreateAssistantMessage adds the message to the backend.
func (a *Server) CreateAssistantMessage(ctx context.Context, msg *assist.CreateAssistantMessageRequest) error {
	return trace.Wrap(a.Services.CreateAssistantMessage(ctx, msg))
}

// UpdateAssistantConversationInfo stores the given conversation title in the backend.
func (a *Server) UpdateAssistantConversationInfo(ctx context.Context, msg *assist.UpdateAssistantConversationInfoRequest) error {
	return trace.Wrap(a.Services.UpdateAssistantConversationInfo(ctx, msg))
}

// CreateAssistantConversation creates a new conversation entry in the backend.
func (a *Server) CreateAssistantConversation(ctx context.Context, req *assist.CreateAssistantConversationRequest) (*assist.CreateAssistantConversationResponse, error) {
	resp, err := a.Services.CreateAssistantConversation(ctx, req)
	return resp, trace.Wrap(err)
}

// GetAssistantConversations returns all conversations started by a user.
func (a *Server) GetAssistantConversations(ctx context.Context, request *assist.GetAssistantConversationsRequest) (*assist.GetAssistantConversationsResponse, error) {
	resp, err := a.Services.GetAssistantConversations(ctx, request)
	return resp, trace.Wrap(err)
}

// DeleteAssistantConversation deletes a conversation from the backend.
func (a *Server) DeleteAssistantConversation(ctx context.Context, request *assist.DeleteAssistantConversationRequest) error {
	return trace.Wrap(a.Services.DeleteAssistantConversation(ctx, request))
}

// CompareAndSwapHeadlessAuthentication performs a compare
// and swap replacement on a headless authentication resource.
func (a *Server) CompareAndSwapHeadlessAuthentication(ctx context.Context, old, new *types.HeadlessAuthentication) (*types.HeadlessAuthentication, error) {
	headlessAuthn, err := a.Services.CompareAndSwapHeadlessAuthentication(ctx, old, new)
	return headlessAuthn, trace.Wrap(err)
}

// getAccessRequestMonthlyUsage returns the number of access requests that have been created this month.
func (a *Server) getAccessRequestMonthlyUsage(ctx context.Context) (int, error) {
	return resourceusage.GetAccessRequestMonthlyUsage(ctx, a.Services.AuditLogSessionStreamer, a.clock.Now().UTC())
}

// verifyAccessRequestMonthlyLimit checks whether the cluster has exceeded the monthly access request limit.
// If so, it returns an error. This is only applicable on usage-based billing plans.
func (a *Server) verifyAccessRequestMonthlyLimit(ctx context.Context) error {
	f := modules.GetModules().Features()
	if !f.IsUsageBasedBilling {
		return nil // unlimited
	}
	monthlyLimit := f.AccessRequests.MonthlyRequestLimit

	const limitReachedMessage = "cluster has reached its monthly access request limit, please contact the cluster administrator"

	usage, err := a.getAccessRequestMonthlyUsage(ctx)
	if err != nil {
		return trace.Wrap(err)
	}
	if usage >= monthlyLimit {
		return trace.AccessDenied(limitReachedMessage)
	}

	return nil
}

// getProxyPublicAddr returns the first valid, non-empty proxy public address it
// finds, or empty otherwise.
func (a *Server) getProxyPublicAddr() string {
	if proxies, err := a.GetProxies(); err == nil {
		for _, p := range proxies {
			addr := p.GetPublicAddr()
			if addr == "" {
				continue
			}
			if _, err := utils.ParseAddr(addr); err != nil {
				log.Warningf("Invalid public address on the proxy %q: %q: %v.", p.GetName(), addr, err)
				continue
			}
			return addr
		}
	}
	return ""
}

// GetNodeStream streams a list of registered servers.
func (a *Server) GetNodeStream(ctx context.Context, namespace string) stream.Stream[types.Server] {
	var done bool
	startKey := ""
	return stream.PageFunc(func() ([]types.Server, error) {
		if done {
			return nil, io.EOF
		}
		resp, err := a.ListResources(ctx, proto.ListResourcesRequest{
			ResourceType: types.KindNode,
			Namespace:    namespace,
			Limit:        apidefaults.DefaultChunkSize,
			StartKey:     startKey,
		})
		if err != nil {
			return nil, trace.Wrap(err)
		}
		startKey = resp.NextKey
		done = startKey == ""
		resources := types.ResourcesWithLabels(resp.Resources)
		servers, err := resources.AsServers()
		return servers, trace.Wrap(err)
	})
}

// authKeepAliver is a keep aliver using auth server directly
type authKeepAliver struct {
	sync.RWMutex
	a           *Server
	ctx         context.Context
	cancel      context.CancelFunc
	keepAlivesC chan types.KeepAlive
	err         error
}

// KeepAlives returns a channel accepting keep alive requests
func (k *authKeepAliver) KeepAlives() chan<- types.KeepAlive {
	return k.keepAlivesC
}

func (k *authKeepAliver) forwardKeepAlives() {
	for {
		select {
		case <-k.a.closeCtx.Done():
			k.Close()
			return
		case <-k.ctx.Done():
			return
		case keepAlive := <-k.keepAlivesC:
			err := k.a.KeepAliveServer(k.ctx, keepAlive)
			if err != nil {
				k.closeWithError(err)
				return
			}
		}
	}
}

func (k *authKeepAliver) closeWithError(err error) {
	k.Close()
	k.Lock()
	defer k.Unlock()
	k.err = err
}

// Error returns the error if keep aliver
// has been closed
func (k *authKeepAliver) Error() error {
	k.RLock()
	defer k.RUnlock()
	return k.err
}

// Done returns channel that is closed whenever
// keep aliver is closed
func (k *authKeepAliver) Done() <-chan struct{} {
	return k.ctx.Done()
}

// Close closes keep aliver and cancels all goroutines
func (k *authKeepAliver) Close() error {
	k.cancel()
	return nil
}

const (
	// BearerTokenTTL specifies standard bearer token to exist before
	// it has to be renewed by the client
	BearerTokenTTL = 10 * time.Minute

	// TokenLenBytes is len in bytes of the invite token
	TokenLenBytes = 16

	// RecoveryTokenLenBytes is len in bytes of a user token for recovery.
	RecoveryTokenLenBytes = 32

	// SessionTokenBytes is the number of bytes of a web or application session.
	SessionTokenBytes = 32
)

// githubClient is internal structure that stores Github OAuth 2client and its config
type githubClient struct {
	client *oauth2.Client
	config oauth2.Config
}

// oauth2ConfigsEqual returns true if the provided OAuth2 configs are equal
func oauth2ConfigsEqual(a, b oauth2.Config) bool {
	if a.Credentials.ID != b.Credentials.ID {
		return false
	}
	if a.Credentials.Secret != b.Credentials.Secret {
		return false
	}
	if a.RedirectURL != b.RedirectURL {
		return false
	}
	if len(a.Scope) != len(b.Scope) {
		return false
	}
	for i := range a.Scope {
		if a.Scope[i] != b.Scope[i] {
			return false
		}
	}
	if a.AuthURL != b.AuthURL {
		return false
	}
	if a.TokenURL != b.TokenURL {
		return false
	}
	if a.AuthMethod != b.AuthMethod {
		return false
	}
	return true
}

// WithClusterCAs returns a TLS hello callback that returns a copy of the provided
// TLS config with client CAs pool of the specified cluster.
func WithClusterCAs(tlsConfig *tls.Config, ap AccessCache, currentClusterName string, log logrus.FieldLogger) func(*tls.ClientHelloInfo) (*tls.Config, error) {
	return func(info *tls.ClientHelloInfo) (*tls.Config, error) {
		var clusterName string
		var err error
		if info.ServerName != "" {
			// Newer clients will set SNI that encodes the cluster name.
			clusterName, err = apiutils.DecodeClusterName(info.ServerName)
			if err != nil {
				if !trace.IsNotFound(err) {
					log.Debugf("Ignoring unsupported cluster name name %q.", info.ServerName)
					clusterName = ""
				}
			}
		}
		pool, totalSubjectsLen, err := DefaultClientCertPool(ap, clusterName)
		if err != nil {
			log.WithError(err).Errorf("Failed to retrieve client pool for %q.", clusterName)
			// this falls back to the default config
			return nil, nil
		}

		// Per https://tools.ietf.org/html/rfc5246#section-7.4.4 the total size of
		// the known CA subjects sent to the client can't exceed 2^16-1 (due to
		// 2-byte length encoding). The crypto/tls stack will panic if this
		// happens.
		//
		// This usually happens on the root cluster with a very large (>500) number
		// of leaf clusters. In these cases, the client cert will be signed by the
		// current (root) cluster.
		//
		// If the number of CAs turns out too large for the handshake, drop all but
		// the current cluster CA. In the unlikely case where it's wrong, the
		// client will be rejected.
		if totalSubjectsLen >= int64(math.MaxUint16) {
			log.Debugf("Number of CAs in client cert pool is too large and cannot be encoded in a TLS handshake; this is due to a large number of trusted clusters; will use only the CA of the current cluster to validate.")

			pool, _, err = DefaultClientCertPool(ap, currentClusterName)
			if err != nil {
				log.WithError(err).Errorf("Failed to retrieve client pool for %q.", currentClusterName)
				// this falls back to the default config
				return nil, nil
			}
		}
		tlsCopy := tlsConfig.Clone()
		tlsCopy.ClientCAs = pool
		return tlsCopy, nil
	}
}

// DefaultDNSNamesForRole returns default DNS names for the specified role.
func DefaultDNSNamesForRole(role types.SystemRole) []string {
	if (types.SystemRoles{role}).IncludeAny(
		types.RoleAuth,
		types.RoleAdmin,
		types.RoleProxy,
		types.RoleKube,
		types.RoleApp,
		types.RoleDatabase,
		types.RoleWindowsDesktop,
		types.RoleOkta,
	) {
		return []string{
			"*." + constants.APIDomain,
			constants.APIDomain,
		}
	}
	return nil
}
