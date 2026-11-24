package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/forwardpolicy"
)

func runForwardOwner(ctx context.Context, cfg bridge.ForwardConfig, imap bridge.IMAPConfig, command string, args []string, output io.Writer) error {
	var limit int
	switch command {
	case "allow", "forget":
		if len(args) != 1 || !bridge.ValidForwardMailbox(args[0]) {
			return errors.New("expected one bare mailbox address")
		}
	case "limits":
		if len(args) > 1 {
			return errors.New("expected at most one daily limit")
		}
		if len(args) == 1 {
			n, err := strconv.Atoi(args[0])
			if err != nil || n < 1 || n > bridge.MaxForwardDailyLimit {
				return fmt.Errorf("daily limit must be an integer from 1 to %d", bridge.MaxForwardDailyLimit)
			}
			limit = n
		}
	}

	creds, err := bridge.LoadCredentials(ctx, imap.CredentialCommand, time.Duration(imap.CommandTimeoutMs)*time.Millisecond)
	if err != nil {
		return err
	}
	defer creds.Zero()

	login := string(creds.Username)
	switch command {
	case "allow":
		return forwardpolicy.Allow(cfg, login, args[0])
	case "forget":
		return forwardpolicy.Forget(cfg, login, args[0])
	}
	if limit != 0 {
		return forwardpolicy.SetLimit(cfg, login, limit)
	}

	policy, err := forwardpolicy.Inspect(cfg, login)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "dailyLimit=%d autoRemember=%t rememberedRecipients=%d/%d\n", policy.DailyLimit, cfg.AutoRememberEnabled(), len(policy.RememberedRecipients), bridge.MaxRememberedRecipients)
	return err
}
