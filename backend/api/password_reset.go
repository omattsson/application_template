package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"backend/internal/config"
	"backend/internal/database"
	"backend/internal/health"
	"backend/internal/mail"
	"backend/internal/models"
)

// The optional mail and password reset modules (MAIL_ENABLED,
// PASSWORD_RESET_ENABLED).
const (
	// passwordResetCleanupInterval is how often the leader deletes the reset
	// tokens that expired or were used long ago.
	passwordResetCleanupInterval = time.Hour
	// passwordResetCleanupBatch is the number of rows of one delete.
	passwordResetCleanupBatch = 1000
	// passwordResetCleanupMaxBatches bounds the deletes of one run, so a run
	// returns soon after its context is cancelled.
	passwordResetCleanupMaxBatches = 100
	// passwordResetUsedRetention is how long a used token stays (the audit
	// trail of the table) before the janitor deletes it.
	passwordResetUsedRetention = 24 * time.Hour
)

// mailTransport is a mailer that can also probe its server for the readiness
// check.
type mailTransport interface {
	mail.Mailer
	Ping(ctx context.Context) error
}

// newMailer builds the mailer of MAIL_ENABLED. Tests replace it.
var newMailer = defaultMailer

// defaultMailer returns the SMTP mailer (no network at construction).
func defaultMailer(cfg config.MailConfig) (mailTransport, error) {
	return mail.NewSMTP(cfg)
}

// buildMail builds the mailer and registers its readiness check when
// MAIL_ENABLED=true. It returns nil with the module off. The check is
// informational: a mail outage shows as DEGRADED and never gates readiness. It
// is cached and bounded (mail.NewHealthCheck), so /health/ready never waits on
// the SMTP server and a probe never runs per request.
func buildMail(cfg *config.Config, checker *health.HealthChecker) (mail.Mailer, error) {
	if !cfg.Mail.Enabled {
		return nil, nil
	}

	m, err := newMailer(cfg.Mail)
	if err != nil {
		return nil, err
	}

	// Informational: a mail outage shows as DEGRADED but never takes the replica
	// out of service (the API works without mail).
	checker.AddInformationalCheck("mail", mail.NewHealthCheck(m.Ping, mail.HealthCacheTTL, mail.HealthProbeTimeout, nil))
	slog.Info("Mail enabled", "smtp_tls", cfg.Mail.TLS, "smtp_port", cfg.Mail.Port)

	return m, nil
}

// passwordResetTokens returns the token repository of PASSWORD_RESET_ENABLED
// on the database of repo; nil with the module off, so nothing touches the
// table.
func passwordResetTokens(cfg *config.Config, repo models.Repository) (models.PasswordResetTokenRepository, error) {
	if !cfg.PasswordReset.Enabled {
		return nil, nil
	}

	g, ok := repo.(gormRepository)
	if !ok || g.DB() == nil {
		return nil, errors.New("PASSWORD_RESET_ENABLED needs the database repository")
	}

	return database.NewGORMPasswordResetTokenRepository(g.DB()), nil
}

// passwordResetCleanup returns the function of the password-reset-cleanup
// worker: it deletes the expired tokens and the ones used longer ago than
// passwordResetUsedRetention, in batches, at most passwordResetCleanupMaxBatches
// per run, and stops at once when ctx ends. It is idempotent and safe on
// several replicas.
func passwordResetCleanup(repo models.PasswordResetTokenRepository) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		total := int64(0)

		for range passwordResetCleanupMaxBatches {
			if err := ctx.Err(); err != nil {
				return err
			}

			n, err := repo.DeleteStale(ctx, passwordResetUsedRetention, passwordResetCleanupBatch)
			total += n

			if err != nil {
				return err
			}

			if n < passwordResetCleanupBatch {
				break
			}
		}

		if total > 0 {
			slog.Info("Deleted stale password reset tokens", "count", total)
		}

		return nil
	}
}
