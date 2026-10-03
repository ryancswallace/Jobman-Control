// Package app composes the Jobman Control process.
package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/ryancswallace/jobman-control/internal/agentca"
	"github.com/ryancswallace/jobman-control/internal/auth"
	"github.com/ryancswallace/jobman-control/internal/config"
	"github.com/ryancswallace/jobman-control/internal/directory"
	"github.com/ryancswallace/jobman-control/internal/domain"
	"github.com/ryancswallace/jobman-control/internal/httpapi"
	"github.com/ryancswallace/jobman-control/internal/store/postgres"
)

// Run loads configuration and serves until ctx is canceled or the listener fails.
func Run(ctx context.Context, logger *slog.Logger) error {
	configuration, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	return run(ctx, logger, configuration)
}

func run(ctx context.Context, logger *slog.Logger, configuration config.Config) error {
	startupContext, cancelStartup := context.WithTimeout(ctx, configuration.ShutdownTimeout)
	defer cancelStartup()
	pool, err := postgres.Open(startupContext, configuration.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if configuration.MigrateOnStart {
		if err = postgres.Migrate(startupContext, pool); err != nil {
			return fmt.Errorf("migrate database: %w", err)
		}
	} else if err = postgres.CheckMigrations(startupContext, pool); err != nil {
		return fmt.Errorf("verify database migrations: %w", err)
	}
	store := postgres.New(pool, configuration.AgentTokenKey)
	var directoryConfig *directory.Config
	if configuration.DirectoryConfigFile != "" {
		loaded, loadErr := directory.Load(configuration.DirectoryConfigFile)
		if loadErr != nil {
			return loadErr
		}
		plan, planErr := store.PlanDirectory(startupContext, loaded.Mapping)
		if planErr != nil {
			return fmt.Errorf("validate directory transition: %w", planErr)
		}
		logger.InfoContext(ctx, "Directory configuration plan", "source-id", plan.SourceID, "revision", plan.Revision, "namespaces", plan.NamespaceCount, "new-managed-namespaces", plan.NewManagedNamespaces, "retained-non-directory-grants", plan.RetainedNonDirectoryGrants, "identities", plan.IdentityCount, "bindings", plan.BindingCount)
		if configuration.DirectoryMode == "preview" {
			return nil
		}
		if err = store.ConfigureDirectory(startupContext, loaded.Mapping); err != nil {
			return fmt.Errorf("configure directory authority: %w", err)
		}
		directoryConfig = &loaded
	}
	var certificateAuthority *agentca.Authority
	if configuration.AgentCACertificateFile != "" {
		certificateAuthority, err = agentca.Load(
			configuration.AgentCACertificateFile, configuration.AgentCAKeyFile,
		)
		if err != nil {
			return fmt.Errorf("load agent certificate authority: %w", err)
		}
	}
	clientAuthenticator, err := configureAuthentication(startupContext, store, configuration)
	if err != nil {
		return err
	}

	var delegationAuthenticator *auth.DelegationAuthenticator
	var delegationCA []byte
	if configuration.DelegationRegistryFile != "" {
		keys, loadErr := auth.LoadDelegationKeys(configuration.DelegationRegistryFile)
		if loadErr != nil {
			return loadErr
		}
		delegationCA, err = readDelegationCA(configuration.DelegationClientCAFile)
		if err != nil {
			return err
		}
		if !x509.NewCertPool().AppendCertsFromPEM(delegationCA) {
			return errors.New("delegation client CA contains no certificates")
		}
		if registerErr := store.RegisterDelegationKeys(ctx, keys); registerErr != nil {
			return fmt.Errorf("apply delegation service registry: %w", registerErr)
		}
		delegationAuthenticator = &auth.DelegationAuthenticator{Registry: store}
	}
	handler, err := httpapi.New(httpapi.Options{
		Repository:               store,
		Authenticator:            clientAuthenticator,
		DelegationAuthenticator:  delegationAuthenticator,
		MaxRequestBytes:          configuration.MaxRequestBytes,
		ReadinessTimeout:         configuration.ReadinessTimeout,
		EnrollmentLifetime:       configuration.EnrollmentLifetime,
		AgentSessionLifetime:     configuration.AgentSessionLifetime,
		AgentCertificateLifetime: configuration.AgentCertificateLifetime,
		CertificateAuthority:     certificateAuthority,
		Logger:                   logger,
	})
	if err != nil {
		return fmt.Errorf("create HTTP API: %w", err)
	}
	listenConfiguration := net.ListenConfig{}
	listener, err := listenConfiguration.Listen(ctx, "tcp", configuration.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen for HTTP API: %w", err)
	}

	tlsConfiguration := &tls.Config{MinVersion: tls.VersionTLS12}
	if certificateAuthority != nil {
		tlsConfiguration.ClientAuth = tls.VerifyClientCertIfGiven
		tlsConfiguration.ClientCAs = certificateAuthority.CertificatePool()
	}
	if len(delegationCA) > 0 {
		if tlsConfiguration.ClientCAs == nil {
			tlsConfiguration.ClientCAs = x509.NewCertPool()
		}
		if !tlsConfiguration.ClientCAs.AppendCertsFromPEM(delegationCA) {
			return errors.New("delegation client CA contains no certificates")
		}
		tlsConfiguration.ClientAuth = tls.VerifyClientCertIfGiven
	}

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: configuration.ReadHeaderTimeout,
		ReadTimeout:       configuration.ReadTimeout,
		WriteTimeout:      configuration.WriteTimeout,
		IdleTimeout:       configuration.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
		TLSConfig:         tlsConfiguration,
	}
	serveErrors := make(chan error, 1)
	go func() {
		if configuration.TLSCertificateFile != "" {
			serveErrors <- server.ServeTLS(
				listener, configuration.TLSCertificateFile, configuration.TLSKeyFile,
			)
			return
		}
		serveErrors <- server.Serve(listener)
	}()
	go runCoordinator(
		ctx, logger, store, configuration.CoordinatorInterval, configuration.AgentStaleAfter, configuration.DelegationAuditRetention,
	)
	if directoryConfig != nil {
		go runDirectory(ctx, logger, store, *directoryConfig)
	}
	logger.InfoContext(
		ctx, "Jobman Control API is listening",
		"address", listener.Addr().String(), "auth-mode", configuration.AuthMode,
	)

	select {
	case serveErr := <-serveErrors:
		if errors.Is(serveErr, http.ErrServerClosed) {
			return nil
		}

		return fmt.Errorf("serve HTTP API: %w", serveErr)
	case <-ctx.Done():
		shutdownContext, cancelShutdown := context.WithTimeout(context.WithoutCancel(ctx), configuration.ShutdownTimeout)
		defer cancelShutdown()
		if shutdownErr := server.Shutdown(shutdownContext); shutdownErr != nil {
			closeErr := server.Close()

			return errors.Join(
				fmt.Errorf("shut down HTTP API: %w", shutdownErr),
				wrapCloseError(closeErr),
			)
		}
		select {
		case serveErr := <-serveErrors:
			if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				return fmt.Errorf("finish HTTP API: %w", serveErr)
			}
		case <-time.After(configuration.ShutdownTimeout):
			return errors.New("HTTP API did not stop after shutdown")
		}

		return nil
	}
}

func configureAuthentication(
	ctx context.Context,
	store *postgres.Store,
	configuration config.Config,
) (auth.Authenticator, error) {
	switch configuration.AuthMode {
	case config.AuthModeDevelopment:
		principal := domain.Principal{
			Issuer: configuration.DevelopmentIssuer, Subject: configuration.DevelopmentSubject,
		}
		if err := store.EnsureBootstrapIdentity(ctx, domain.BootstrapIdentity{
			Principal: principal, DisplayName: configuration.DevelopmentName,
			Namespace: configuration.DevelopmentNamespace, Mode: "development",
		}); err != nil {
			return nil, err
		}

		return auth.DevelopmentAuthenticator{Principal: principal}, nil
	case config.AuthModeOIDC:
		if configuration.BootstrapSubject != "" {
			if err := store.EnsureBootstrapIdentity(ctx, domain.BootstrapIdentity{
				Principal: domain.Principal{
					Issuer: configuration.OIDCIssuer, Subject: configuration.BootstrapSubject,
				},
				DisplayName: configuration.BootstrapName,
				Namespace:   configuration.BootstrapNamespace,
				Mode:        "oidc",
			}); err != nil {
				return nil, err
			}
		}
		oidcAuthenticator, err := auth.DiscoverOIDC(
			ctx, configuration.OIDCIssuer, configuration.OIDCAudience,
			&http.Client{Timeout: 10 * time.Second},
		)
		if err != nil {
			return nil, err
		}

		return oidcAuthenticator, nil
	default:
		return nil, fmt.Errorf("unsupported authentication mode %q", configuration.AuthMode)
	}
}

type assignmentReconciler interface {
	ReconcileAssignments(context.Context, int) (int, error)
	ReconcileStaleExecutions(context.Context, time.Duration, int) (int, error)
	PruneOperationalData(context.Context, int) (int, error)
	PruneDelegationAudits(context.Context, int, time.Duration) (int, error)
	PruneDirectoryAudits(context.Context, int, time.Duration) (int, error)
}

func runCoordinator(
	ctx context.Context,
	logger *slog.Logger,
	reconciler assignmentReconciler,
	interval time.Duration,
	staleAfter time.Duration,
	auditRetention time.Duration,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		created, err := reconciler.ReconcileAssignments(ctx, 32)
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.ErrorContext(ctx, "assignment reconciliation failed")
		}
		if created > 0 {
			logger.InfoContext(ctx, "assignments materialized", "count", created)
		}
		stale, staleErr := reconciler.ReconcileStaleExecutions(ctx, staleAfter, 32)
		if staleErr != nil && !errors.Is(staleErr, context.Canceled) {
			logger.ErrorContext(ctx, "stale execution reconciliation failed")
		}
		if stale > 0 {
			logger.WarnContext(ctx, "execution observations marked stale", "count", stale)
		}
		pruned, pruneErr := reconciler.PruneOperationalData(ctx, 256)
		if pruneErr != nil && !errors.Is(pruneErr, context.Canceled) {
			logger.ErrorContext(ctx, "operational retention failed")
		}
		if pruned > 0 {
			logger.InfoContext(ctx, "expired operational records pruned", "count", pruned)
		}
		if _, auditErr := reconciler.PruneDelegationAudits(ctx, 256, auditRetention); auditErr != nil && !errors.Is(auditErr, context.Canceled) {
			logger.ErrorContext(ctx, "delegation audit retention failed")
		}
		if _, auditErr := reconciler.PruneDirectoryAudits(ctx, 256, auditRetention); auditErr != nil && !errors.Is(auditErr, context.Canceled) {
			logger.ErrorContext(ctx, "directory audit retention failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func wrapCloseError(err error) error {
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}

	return fmt.Errorf("force close HTTP API: %w", err)
}

func readDelegationCA(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open delegation client CA")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 {
		return nil, errors.New("delegation client CA is unreadable or too large")
	}
	return data, nil
}
