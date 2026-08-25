// Package app composes the Jobman Control process.
package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/ryancswallace/jobman-control/internal/agentca"
	"github.com/ryancswallace/jobman-control/internal/auth"
	"github.com/ryancswallace/jobman-control/internal/config"
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

	handler, err := httpapi.New(httpapi.Options{
		Repository:               store,
		Authenticator:            clientAuthenticator,
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
		ctx, logger, store, configuration.CoordinatorInterval, configuration.AgentStaleAfter,
	)
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
}

func runCoordinator(
	ctx context.Context,
	logger *slog.Logger,
	reconciler assignmentReconciler,
	interval time.Duration,
	staleAfter time.Duration,
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
