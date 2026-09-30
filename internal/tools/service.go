package tools

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Core/internal/apperr"
	"github.com/AIShie-Education/AIShie-Core/internal/auth"
	"github.com/AIShie-Education/AIShie-Core/internal/db/dbq"
	"github.com/AIShie-Education/AIShie-Core/internal/domain"
	"github.com/AIShie-Education/AIShie-Core/internal/events"
	"github.com/AIShie-Education/AIShie-Core/internal/ids"
	"github.com/AIShie-Education/AIShie-Core/internal/tool"
)

// A site service (docs/schema.md §2.1, Services) is a program of the site's
// that Core gives an identity for one thing, and for nothing else: the
// runtime's transcriber, which writes documents' text versions
// (document_text.*). It is an actor of kind service, one for each scope,
// made the first time a credential is issued for it; it is seated in no
// course, signs in nowhere, and calls its own tools and nothing else. The
// platform's administrators issue, list and revoke its credentials here;
// the tools that manage actors do not reach it.

func serviceTools() []tool.Tool {
	return []tool.Tool{serviceIssueCredential(), serviceListCredentials(), serviceRevokeCredential()}
}

const (
	EventServiceCredentialIssued  = "service.credential_issued"
	EventServiceCredentialRevoked = "service.credential_revoked"

	// MaxServiceCredentials bounds the live credentials of one service: a
	// runtime's, and a new one's while it takes over.
	MaxServiceCredentials = 5
)

// services names each scope's service, as its actions show it.
var services = map[string]string{domain.ServiceDocumentText: "Transcription service"}

func serviceTarget(scope string) (tool.Target, error) {
	if _, ok := services[scope]; !ok {
		return tool.Target{}, apperr.Invalid("scope must be %s", domain.ServiceDocumentText)
	}
	return tool.Target{Type: "service"}, nil
}

type ServiceIssueCredentialIn struct {
	Scope         string `json:"scope" jsonschema:"the service: document_text, the runtime's transcriber, which writes documents' text versions"`
	Label         string `json:"label" jsonschema:"what this credential is for, so it can be recognised later: the runtime it is given to"`
	ExpiresInDays *int   `json:"expires_in_days,omitempty" jsonschema:"1 to 3650; omit for a credential that does not expire"`
	Replace       bool   `json:"replace,omitempty" jsonschema:"revoke the service's other credentials in the same call, and put back in the queue what they had claimed"`
}

type ServiceIssueCredentialOut struct {
	ServiceActorID uuid.UUID   `json:"service_actor_id" jsonschema:"the service, as its actions name it"`
	CredentialID   uuid.UUID   `json:"credential_id"`
	Token          string      `json:"token" jsonschema:"shown once; it is not stored, and a replay of this call comes back without it"`
	TokenPrefix    string      `json:"token_prefix"`
	ExpiresAt      *time.Time  `json:"expires_at,omitempty"`
	Revoked        []uuid.UUID `json:"revoked,omitempty" jsonschema:"the credentials replaced, with replace"`
}

func serviceIssueCredential() tool.Tool {
	return tool.Define(tool.Spec[ServiceIssueCredentialIn, ServiceIssueCredentialOut]{
		Name: "service.issue_credential",
		Description: "Issue a credential for a site service: document_text, the runtime's transcriber, which takes the " +
			"versions waiting to be transcribed and writes their text (document_text.queue, .complete). The service calls " +
			"its own tools with it, over REST, and nothing else: no other tool, and not the agents' MCP door. The service " +
			"is made the first time; it is seated in no course and signs in nowhere. The credential is returned once and " +
			"only its hash is kept. replace revokes the service's other credentials at once, and puts back in the queue " +
			"what they had claimed; without it a service holds at most " + strconv.Itoa(MaxServiceCredentials) + " (too_many_credentials). " +
			"For platform administrators.",
		Kind: tool.Write, Gate: admins,
		HTTP:      tool.Route{Method: "POST", Pattern: "/v1/services/{scope}/credentials"},
		SecretOut: []string{"token"},
		Resolve: func(_ context.Context, _ dbq.Querier, in ServiceIssueCredentialIn) (tool.Target, error) {
			return serviceTarget(in.Scope)
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in ServiceIssueCredentialIn) (ServiceIssueCredentialOut, error) {
			expires, err := tokenExpiry(in.Label, in.ExpiresInDays, ec.Now)
			if err != nil {
				return ServiceIssueCredentialOut{}, err
			}
			if err := ec.Q.InsertServiceActor(ctx, dbq.InsertServiceActorParams{ID: ids.New(), DisplayName: services[in.Scope],
				Scope: &in.Scope, CreatedByActorID: &ec.Actor.ID, CreatedAt: ec.Now}); err != nil {
				return ServiceIssueCredentialOut{}, err
			}
			svc, err := ec.Q.GetServiceActor(ctx, &in.Scope)
			if err != nil {
				return ServiceIssueCredentialOut{}, err
			}
			// One issue at a time for a service, so that the count holds.
			if err := ec.Q.LockServiceActor(ctx, svc.ID); err != nil {
				return ServiceIssueCredentialOut{}, err
			}
			live, err := ec.Q.ListLiveServiceCredentials(ctx, dbq.ListLiveServiceCredentialsParams{ActorID: svc.ID, Now: &ec.Now})
			if err != nil {
				return ServiceIssueCredentialOut{}, err
			}
			out := ServiceIssueCredentialOut{ServiceActorID: svc.ID, ExpiresAt: expires}
			switch {
			case in.Replace:
				for _, id := range live {
					if _, err := revokeServiceCredential(ctx, ec, svc.ID, in.Scope, id); err != nil {
						return ServiceIssueCredentialOut{}, err
					}
					out.Revoked = append(out.Revoked, id)
				}
			case len(live) >= MaxServiceCredentials:
				return ServiceIssueCredentialOut{}, apperr.Precondition("the service holds %d live credentials already: revoke one, or replace them",
					len(live)).With("reason", "too_many_credentials")
			}
			tok, err := auth.NewServiceToken()
			if err != nil {
				return ServiceIssueCredentialOut{}, err
			}
			out.CredentialID = ids.New()
			label := in.Label
			if err := ec.Q.InsertCredential(ctx, dbq.InsertCredentialParams{ID: out.CredentialID, ActorID: svc.ID, Kind: auth.KindService,
				SecretHash: &tok.Hash, TokenPrefix: &tok.Prefix, Label: &label, ExpiresAt: expires, CreatedAt: ec.Now,
				IssuedByActorID: &ec.Actor.ID}); err != nil {
				return ServiceIssueCredentialOut{}, err
			}
			out.Token, out.TokenPrefix = tok.Full, tok.Prefix
			ec.Emit(events.Event{Type: EventServiceCredentialIssued, SubjectType: "actor", SubjectID: &svc.ID,
				Payload: map[string]any{"scope": in.Scope, "credential_id": out.CredentialID, "replaced": len(out.Revoked)}})
			return out, nil
		},
	})
}

// revokeServiceCredential revokes one of a service's live credentials, and
// puts back in the queue what it had claimed: its claims end with it, and
// another credential may take them at once. It says how many it gave back,
// and whether it revoked anything.
func revokeServiceCredential(ctx context.Context, ec *tool.ExecCtx, service uuid.UUID, scope string, credential uuid.UUID) (int, error) {
	n, err := ec.Q.RevokeServiceCredential(ctx, dbq.RevokeServiceCredentialParams{ID: credential, ActorID: service, Now: &ec.Now})
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, apperr.Missing("no such live credential of the service")
	}
	var released []uuid.UUID
	if scope == domain.ServiceDocumentText {
		if released, err = ec.Q.ReleaseTexts(ctx, dbq.ReleaseTextsParams{CredentialID: &credential, Now: ec.Now}); err != nil {
			return 0, err
		}
		if err := notifyQueued(ctx, ec.Q, released...); err != nil {
			return 0, err
		}
	}
	ec.Emit(events.Event{Type: EventServiceCredentialRevoked, SubjectType: "actor", SubjectID: &service,
		Payload: map[string]any{"scope": scope, "credential_id": credential, "claims_released": len(released)}})
	return len(released), nil
}

type ServiceScopeIn struct {
	Scope string `json:"scope" jsonschema:"the service: document_text, the runtime's transcriber, which writes documents' text versions"`
}

type ServiceCredentialView struct {
	ID          uuid.UUID  `json:"id"`
	TokenPrefix *string    `json:"token_prefix,omitempty" jsonschema:"the public part of the credential, enough to tell which one it is"`
	Label       *string    `json:"label,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty" jsonschema:"when the service last called with it, to the minute"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	IssuedByID  *uuid.UUID `json:"issued_by_actor_id,omitempty"`
	IssuedBy    *string    `json:"issued_by_name,omitempty"`
	Live        bool       `json:"live" jsonschema:"neither revoked nor expired: the service can call with it now"`
	ClaimsHeld  int32      `json:"claims_held" jsonschema:"how many text versions it has claimed and not finished, now"`
}

type ServiceListCredentialsOut struct {
	Scope          string                  `json:"scope"`
	ServiceActorID *uuid.UUID              `json:"service_actor_id,omitempty" jsonschema:"absent until a credential is first issued for the service"`
	Credentials    []ServiceCredentialView `json:"credentials"`
}

func serviceListCredentials() tool.Tool {
	return tool.Define(tool.Spec[ServiceScopeIn, ServiceListCredentialsOut]{
		Name: "service.list_credentials",
		Description: "A site service's credentials, newest first, revoked ones included, with their label, prefix, issuer, " +
			"expiry, last use, whether they are live, and how many text versions each has claimed and not finished. " +
			"Secrets are never shown. For platform administrators.",
		Kind: tool.Read, Gate: admins,
		HTTP: tool.Route{Method: "GET", Pattern: "/v1/services/{scope}/credentials"},
		Resolve: func(_ context.Context, _ dbq.Querier, in ServiceScopeIn) (tool.Target, error) {
			return serviceTarget(in.Scope)
		},
		Query: func(ctx context.Context, rc *tool.ReadCtx, in ServiceScopeIn) (ServiceListCredentialsOut, error) {
			out := ServiceListCredentialsOut{Scope: in.Scope, Credentials: []ServiceCredentialView{}}
			svc, err := rc.Q.GetServiceActor(ctx, &in.Scope)
			if errors.Is(err, pgx.ErrNoRows) {
				return out, nil
			}
			if err != nil {
				return out, err
			}
			out.ServiceActorID = &svc.ID
			rows, err := rc.Q.ListServiceCredentials(ctx, svc.ID)
			for _, r := range rows {
				out.Credentials = append(out.Credentials, ServiceCredentialView{ID: r.ID, TokenPrefix: r.TokenPrefix, Label: r.Label,
					LastUsedAt: r.LastUsedAt, ExpiresAt: r.ExpiresAt, RevokedAt: r.RevokedAt, CreatedAt: r.CreatedAt,
					IssuedByID: r.IssuedByActorID, IssuedBy: r.IssuedByName, ClaimsHeld: r.ClaimsHeld,
					Live: r.RevokedAt == nil && (r.ExpiresAt == nil || r.ExpiresAt.After(rc.Now))})
			}
			return out, err
		},
	})
}

type ServiceRevokeCredentialIn struct {
	Scope        string    `json:"scope" jsonschema:"the service: document_text, the runtime's transcriber, which writes documents' text versions"`
	CredentialID uuid.UUID `json:"credential_id"`
}

type ServiceRevokeCredentialOut struct {
	OK             bool `json:"ok"`
	ClaimsReleased int  `json:"claims_released" jsonschema:"how many text versions it had claimed, now back in the queue"`
}

func serviceRevokeCredential() tool.Tool {
	return tool.Define(tool.Spec[ServiceRevokeCredentialIn, ServiceRevokeCredentialOut]{
		Name: "service.revoke_credential",
		Description: "Revoke a site service's credential: it stops working at once, a call of the service's waiting on it " +
			"included, and what it had claimed goes back in the queue for another. For platform administrators.",
		Kind: tool.Write, Gate: admins,
		HTTP: tool.Route{Method: "POST", Pattern: "/v1/services/{scope}/credentials/{credential_id}/revoke"},
		Resolve: func(_ context.Context, _ dbq.Querier, in ServiceRevokeCredentialIn) (tool.Target, error) {
			t, err := serviceTarget(in.Scope)
			t.Type, t.ID = "credential", &in.CredentialID
			return t, err
		},
		Execute: func(ctx context.Context, ec *tool.ExecCtx, in ServiceRevokeCredentialIn) (ServiceRevokeCredentialOut, error) {
			svc, err := ec.Q.GetServiceActor(ctx, &in.Scope)
			if errors.Is(err, pgx.ErrNoRows) {
				return ServiceRevokeCredentialOut{}, apperr.Missing("no such live credential of the service")
			}
			if err != nil {
				return ServiceRevokeCredentialOut{}, err
			}
			if err := ec.Q.LockServiceActor(ctx, svc.ID); err != nil {
				return ServiceRevokeCredentialOut{}, err
			}
			n, err := revokeServiceCredential(ctx, ec, svc.ID, in.Scope, in.CredentialID)
			if err != nil {
				return ServiceRevokeCredentialOut{}, err
			}
			return ServiceRevokeCredentialOut{OK: true, ClaimsReleased: n}, nil
		},
	})
}
