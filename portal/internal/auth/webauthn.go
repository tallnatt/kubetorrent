package auth

import (
	"fmt"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/tallnatt/kubetorrent/portal/internal/config"
)

func NewWebAuthn(cfg *config.Config) (*webauthn.WebAuthn, error) {
	wconfig := &webauthn.Config{
		RPDisplayName: cfg.RPDisplayName,
		RPID:          cfg.RPID,
		RPOrigins:     cfg.RPOrigins,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationPreferred,
		},
	}
	wa, err := webauthn.New(wconfig)
	if err != nil {
		return nil, fmt.Errorf("init webauthn: %w", err)
	}
	return wa, nil
}
