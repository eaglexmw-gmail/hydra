// Copyright © 2022 Ory Corp
// SPDX-License-Identifier: Apache-2.0

package sql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/gobuffalo/pop/v6"
	"github.com/gofrs/uuid"

	"github.com/ory/hydra/v2/oauth2/flowctx"
	"github.com/ory/x/otelx"
	"github.com/ory/x/sqlxx"

	"github.com/ory/x/errorsx"

	"github.com/pkg/errors"

	"github.com/ory/fosite"
	"github.com/ory/hydra/v2/client"
	"github.com/ory/hydra/v2/consent"
	"github.com/ory/hydra/v2/flow"
	"github.com/ory/hydra/v2/x"
	"github.com/ory/x/sqlcon"
)

var _ consent.Manager = &Persister{}

func (p *Persister) RevokeSubjectConsentSession(ctx context.Context, user string) error {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.RevokeSubjectConsentSession")
	defer span.End()

	return p.transaction(ctx, p.revokeConsentSession("consent_challenge_id IS NOT NULL AND subject = ?", user))
}

func (p *Persister) RevokeSubjectClientConsentSession(ctx context.Context, user, client string) error {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.RevokeSubjectClientConsentSession")
	defer span.End()

	return p.transaction(ctx, p.revokeConsentSession("consent_challenge_id IS NOT NULL AND subject = ? AND client_id = ?", user, client))
}

func (p *Persister) revokeConsentSession(whereStmt string, whereArgs ...interface{}) func(context.Context, *pop.Connection) error {
	return func(ctx context.Context, c *pop.Connection) error {
		fs := make([]*flow.Flow, 0)
		if err := p.QueryWithNetwork(ctx).
			Where(whereStmt, whereArgs...).
			Select("consent_challenge_id").
			All(&fs); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errorsx.WithStack(x.ErrNotFound)
			}

			return sqlcon.HandleError(err)
		}

		var count int
		for _, f := range fs {
			if err := p.RevokeAccessToken(ctx, f.ConsentChallengeID.String()); errors.Is(err, fosite.ErrNotFound) {
				// do nothing
			} else if err != nil {
				return err
			}

			if err := p.RevokeRefreshToken(ctx, f.ConsentChallengeID.String()); errors.Is(err, fosite.ErrNotFound) {
				// do nothing
			} else if err != nil {
				return err
			}

			localCount, err := c.RawQuery("DELETE FROM hydra_oauth2_flow WHERE consent_challenge_id = ? AND nid = ?", f.ConsentChallengeID, p.NetworkID(ctx)).ExecWithCount()
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return errorsx.WithStack(x.ErrNotFound)
				}
				return sqlcon.HandleError(err)
			}

			// If there are no sessions to revoke we should return an error to indicate to the caller
			// that the request failed.
			count += localCount
		}

		if count == 0 {
			return errorsx.WithStack(x.ErrNotFound)
		}

		return nil
	}
}

func (p *Persister) RevokeSubjectLoginSession(ctx context.Context, subject string) error {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.RevokeSubjectLoginSession")
	defer span.End()

	err := p.QueryWithNetwork(ctx).Where("subject = ?", subject).Delete(&flow.LoginSession{})
	if err != nil {
		return sqlcon.HandleError(err)
	}

	// This confuses people, see https://github.com/ory/hydra/issues/1168
	//
	// count, _ := rows.RowsAffected()
	// if count == 0 {
	// 	 return errorsx.WithStack(x.ErrNotFound)
	// }

	return nil
}

func (p *Persister) CreateForcedObfuscatedLoginSession(ctx context.Context, session *consent.ForcedObfuscatedLoginSession) error {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.CreateForcedObfuscatedLoginSession")
	defer span.End()

	return p.transaction(ctx, func(ctx context.Context, c *pop.Connection) error {
		nid := p.NetworkID(ctx)
		if err := c.RawQuery(
			"DELETE FROM hydra_oauth2_obfuscated_authentication_session WHERE nid = ? AND client_id = ? AND subject = ?",
			nid,
			session.ClientID,
			session.Subject,
		).Exec(); err != nil {
			return sqlcon.HandleError(err)
		}

		return sqlcon.HandleError(c.RawQuery(
			"INSERT INTO hydra_oauth2_obfuscated_authentication_session (nid, subject, client_id, subject_obfuscated) VALUES (?, ?, ?, ?)",
			nid,
			session.Subject,
			session.ClientID,
			session.SubjectObfuscated,
		).Exec())
	})
}

func (p *Persister) GetForcedObfuscatedLoginSession(ctx context.Context, client, obfuscated string) (*consent.ForcedObfuscatedLoginSession, error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.GetForcedObfuscatedLoginSession")
	defer span.End()

	var s consent.ForcedObfuscatedLoginSession

	if err := p.Connection(ctx).Where(
		"client_id = ? AND subject_obfuscated = ? AND nid = ?",
		client,
		obfuscated,
		p.NetworkID(ctx),
	).First(&s); errors.Is(err, sql.ErrNoRows) {
		return nil, errorsx.WithStack(x.ErrNotFound)
	} else if err != nil {
		return nil, sqlcon.HandleError(err)
	}

	return &s, nil
}

// CreateConsentRequest configures fields that are introduced or changed in the
// consent request. It doesn't touch fields that would be copied from the login
// request.
func (p *Persister) CreateConsentRequest(ctx context.Context, f *flow.Flow, req *flow.OAuth2ConsentRequest) error {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.CreateConsentRequest")
	defer span.End()

	if f == nil {
		return errorsx.WithStack(x.ErrNotFound.WithDebug("Flow is nil"))
	}
	if f.ID != req.LoginChallenge.String() || f.NID != p.NetworkID(ctx) {
		return errorsx.WithStack(x.ErrNotFound)
	}
	f.State = flow.FlowStateConsentInitialized
	f.ConsentChallengeID = sqlxx.NullString(req.ID)
	f.ConsentSkip = req.Skip
	f.ConsentVerifier = sqlxx.NullString(req.Verifier)
	f.ConsentCSRF = sqlxx.NullString(req.CSRF)

	return nil
}

func (p *Persister) GetFlowByConsentChallenge(ctx context.Context, challenge string) (*flow.Flow, error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.GetFlowByConsentChallenge")
	defer span.End()

	// challenge contains the flow.
	f, err := flowctx.Decode[flow.Flow](ctx, p.r.FlowCipher(), challenge, flowctx.AsConsentChallenge)
	if err != nil {
		return nil, errorsx.WithStack(x.ErrNotFound)
	}
	if f.NID != p.NetworkID(ctx) {
		return nil, errorsx.WithStack(x.ErrNotFound)
	}
	if f.RequestedAt.Add(p.config.ConsentRequestMaxAge(ctx)).Before(time.Now()) {
		return nil, errorsx.WithStack(fosite.ErrRequestUnauthorized.WithHint("The consent request has expired, please try again."))
	}

	return f, nil
}

func (p *Persister) GetConsentRequest(ctx context.Context, challenge string) (*flow.OAuth2ConsentRequest, error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.GetConsentRequest")
	defer span.End()

	f, err := p.GetFlowByConsentChallenge(ctx, challenge)
	if err != nil {
		if errors.Is(err, sqlcon.ErrNoRows) {
			return nil, errorsx.WithStack(x.ErrNotFound)
		}
		return nil, err
	}

	// We need to overwrite the ID with the encoded flow (challenge) so that the client is not confused.
	f.ConsentChallengeID = sqlxx.NullString(challenge)

	return f.GetConsentRequest(), nil
}

func (p *Persister) CreateLoginRequest(ctx context.Context, req *flow.LoginRequest) (*flow.Flow, error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.CreateLoginRequest")
	defer span.End()

	f := flow.NewFlow(req)
	nid := p.NetworkID(ctx)
	if nid == uuid.Nil {
		return nil, errorsx.WithStack(x.ErrNotFound)
	}
	f.NID = nid

	return f, nil
}

func (p *Persister) GetFlow(ctx context.Context, loginChallenge string) (*flow.Flow, error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.GetFlow")
	defer span.End()

	var f flow.Flow
	if err := p.QueryWithNetwork(ctx).Where("login_challenge = ?", loginChallenge).First(&f); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errorsx.WithStack(x.ErrNotFound)
		}
		return nil, sqlcon.HandleError(err)
	}
	return &f, nil
}

func (p *Persister) GetLoginRequest(ctx context.Context, loginChallenge string) (*flow.LoginRequest, error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.GetLoginRequest")
	defer span.End()

	f, err := flowctx.Decode[flow.Flow](ctx, p.r.FlowCipher(), loginChallenge, flowctx.AsLoginChallenge)
	if err != nil {
		return nil, errorsx.WithStack(x.ErrNotFound.WithWrap(err))
	}
	if f.NID != p.NetworkID(ctx) {
		return nil, errorsx.WithStack(x.ErrNotFound)
	}
	if f.RequestedAt.Add(p.config.ConsentRequestMaxAge(ctx)).Before(time.Now()) {
		return nil, errorsx.WithStack(fosite.ErrRequestUnauthorized.WithHint("The login request has expired, please try again."))
	}
	lr := f.GetLoginRequest()
	// Restore the short challenge ID, which was previously sent to the encoded flow,
	// to make sure that the challenge ID in the returned flow matches the param.
	lr.ID = loginChallenge

	return lr, nil
}

func (p *Persister) HandleConsentRequest(ctx context.Context, f *flow.Flow, r *flow.AcceptOAuth2ConsentRequest) (*flow.OAuth2ConsentRequest, error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.HandleConsentRequest")
	defer span.End()

	if f == nil {
		return nil, errorsx.WithStack(fosite.ErrInvalidRequest.WithDebug("Flow was nil"))
	}
	if f.NID != p.NetworkID(ctx) {
		return nil, errorsx.WithStack(x.ErrNotFound)
	}
	// Restore the short challenge ID, which was previously sent to the encoded flow,
	// to make sure that the challenge ID in the returned flow matches the param.
	r.ID = f.ConsentChallengeID.String()
	if err := f.HandleConsentRequest(r); err != nil {
		return nil, errorsx.WithStack(err)
	}

	return f.GetConsentRequest(), nil
}

func (p *Persister) VerifyAndInvalidateConsentRequest(ctx context.Context, f *flow.Flow, verifier string) (*flow.AcceptOAuth2ConsentRequest, error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.VerifyAndInvalidateConsentRequest")
	defer span.End()

	if f == nil {
		return nil, errorsx.WithStack(fosite.ErrInvalidRequest.WithDebug("Flow was nil"))
	}
	if f.NID != p.NetworkID(ctx) {
		return nil, errorsx.WithStack(sqlcon.ErrNoRows)
	}

	updatedFlow, err := flowctx.Decode[flow.Flow](ctx, p.r.FlowCipher(), verifier, flowctx.AsConsentVerifier)
	if err != nil {
		return nil, errorsx.WithStack(fosite.ErrAccessDenied.WithHint("The consent verifier has already been used, has not been granted, or is invalid."))
	}
	if updatedFlow.ID != f.ID {
		return nil, errorsx.WithStack(fosite.ErrInvalidRequest.WithDebug("Consent verifier does not match login request."))
	}
	if updatedFlow.NID != p.NetworkID(ctx) {
		return nil, errorsx.WithStack(sqlcon.ErrNoRows)
	}

	// Update flow from login request, but keep requested at.
	updatedFlow.NID = f.NID
	updatedFlow.ConsentCSRF = f.ConsentCSRF
	updatedFlow.ConsentVerifier = f.ConsentVerifier
	*f = *updatedFlow

	if err = f.InvalidateConsentRequest(); err != nil {
		return nil, errorsx.WithStack(fosite.ErrInvalidRequest.WithDebug(err.Error()))
	}

	// We set the consent challenge ID to a new UUID that we can use as a foreign key in the database
	// without encoding the whole flow.
	f.ConsentChallengeID = sqlxx.NullString(uuid.Must(uuid.NewV4()).String())

	if err = p.Connection(ctx).Create(f); err != nil {
		return nil, sqlcon.HandleError(err)
	}

	return f.GetHandledConsentRequest(), nil
}

func (p *Persister) HandleLoginRequest(ctx context.Context, f *flow.Flow, challenge string, r *flow.HandledLoginRequest) (lr *flow.LoginRequest, err error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.HandleLoginRequest")
	defer span.End()

	if f == nil {
		return nil, errorsx.WithStack(fosite.ErrInvalidRequest.WithDebug("Flow was nil"))
	}
	if f.NID != p.NetworkID(ctx) {
		return nil, errorsx.WithStack(x.ErrNotFound)
	}
	r.ID = f.ID
	err = f.HandleLoginRequest(r)
	if err != nil {
		return nil, err
	}

	return p.GetLoginRequest(ctx, challenge)
}

func (p *Persister) VerifyAndInvalidateLoginRequest(ctx context.Context, f *flow.Flow, verifier string) (*flow.HandledLoginRequest, error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.VerifyAndInvalidateLoginRequest")
	defer span.End()

	if f == nil {
		return nil, errorsx.WithStack(fosite.ErrInvalidRequest.WithDebug("Flow was nil"))
	}
	if f.NID != p.NetworkID(ctx) {
		return nil, errorsx.WithStack(sqlcon.ErrNoRows)
	}

	updatedFlow, err := flowctx.Decode[flow.Flow](ctx, p.r.FlowCipher(), verifier, flowctx.AsLoginVerifier)
	if err != nil {
		return nil, errorsx.WithStack(sqlcon.ErrNoRows)
	}
	if f.NID != updatedFlow.NID {
		return nil, errorsx.WithStack(sqlcon.ErrNoRows)
	}

	if updatedFlow.ID != f.ID {
		return nil, errorsx.WithStack(fosite.ErrInvalidRequest.WithDebug("Login verifier does not match login request."))
	}

	// Update flow from login request, but keep requested at.
	updatedFlow.NID = f.NID
	updatedFlow.RequestedAt = f.RequestedAt
	updatedFlow.LoginCSRF = f.LoginCSRF
	updatedFlow.LoginVerifier = f.LoginVerifier
	*f = *updatedFlow

	if err := f.InvalidateLoginRequest(); err != nil {
		return nil, errorsx.WithStack(fosite.ErrInvalidRequest.WithDebug(err.Error()))
	}
	d := f.GetHandledLoginRequest()

	return &d, nil
}

func (p *Persister) GetRememberedLoginSession(ctx context.Context, loginSessionFromCookie *flow.LoginSession, id string) (*flow.LoginSession, error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.GetRememberedLoginSession")
	defer span.End()

	if s := loginSessionFromCookie; s != nil && s.NID == p.NetworkID(ctx) && s.ID == id && s.Remember {
		return s, nil
	}

	var s flow.LoginSession

	if err := p.QueryWithNetwork(ctx).Where("remember = TRUE").Find(&s, id); errors.Is(err, sql.ErrNoRows) {
		return nil, errorsx.WithStack(x.ErrNotFound)
	} else if err != nil {
		return nil, sqlcon.HandleError(err)
	}

	return &s, nil
}

func (p *Persister) ConfirmLoginSession(ctx context.Context, session *flow.LoginSession, id string, authenticatedAt time.Time, subject string, remember bool) error {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.ConfirmLoginSession")
	defer span.End()

	// Since we previously cached the login session, we now need to persist it to db.
	if session != nil {
		if session.NID != p.NetworkID(ctx) || session.ID != id {
			return errorsx.WithStack(x.ErrNotFound)
		}
		session.AuthenticatedAt = sqlxx.NullTime(authenticatedAt.Truncate(time.Second))
		session.Subject = subject
		session.Remember = remember

		return p.CreateWithNetwork(ctx, session)
	}

	// In some unit tests, we still confirm the login session without data from the cookie. We can remove this case
	// once all tests are fixed.
	n, err := p.Connection(ctx).Where("id = ? AND nid = ?", id, p.NetworkID(ctx)).UpdateQuery(&flow.LoginSession{
		AuthenticatedAt: sqlxx.NullTime(authenticatedAt),
		Subject:         subject,
		Remember:        remember,
	}, "authenticated_at", "subject", "remember")
	if err != nil {
		return sqlcon.HandleError(err)
	}
	if n == 0 {
		return errorsx.WithStack(x.ErrNotFound)
	}
	return nil
}

func (p *Persister) CreateLoginSession(ctx context.Context, session *flow.LoginSession) error {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.CreateLoginSession")
	defer span.End()

	nid := p.NetworkID(ctx)
	if nid == uuid.Nil {
		return errorsx.WithStack(x.ErrNotFound)
	}
	session.NID = nid

	return nil
}

func (p *Persister) DeleteLoginSession(ctx context.Context, id string) (deletedSession *flow.LoginSession, err error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.DeleteLoginSession")
	defer otelx.End(span, &err)

	if p.Connection(ctx).Dialect.Name() == "mysql" {
		// MySQL does not support RETURNING.
		return p.mySQLDeleteLoginSession(ctx, id)
	}

	var session flow.LoginSession

	err = p.Connection(ctx).RawQuery(
		`DELETE FROM hydra_oauth2_authentication_session
       WHERE id = ? AND nid = ?
       RETURNING *`,
		id,
		p.NetworkID(ctx),
	).First(&session)
	if err != nil {
		return nil, sqlcon.HandleError(err)
	}

	return &session, nil
}

func (p *Persister) mySQLDeleteLoginSession(ctx context.Context, id string) (*flow.LoginSession, error) {
	var session flow.LoginSession

	err := p.Connection(ctx).Transaction(func(tx *pop.Connection) error {
		err := tx.RawQuery(`
SELECT * FROM hydra_oauth2_authentication_session
WHERE id = ? AND nid = ?`,
			id,
			p.NetworkID(ctx),
		).First(&session)
		if err != nil {
			return err
		}

		return p.Connection(ctx).RawQuery(`
DELETE FROM hydra_oauth2_authentication_session
WHERE id = ? AND nid = ?`,
			id,
			p.NetworkID(ctx),
		).Exec()
	})

	if err != nil {
		return nil, sqlcon.HandleError(err)
	}

	return &session, nil

}

func (p *Persister) FindGrantedAndRememberedConsentRequests(ctx context.Context, client, subject string) (rs []flow.AcceptOAuth2ConsentRequest, err error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.FindGrantedAndRememberedConsentRequests")
	defer span.End()

	var f flow.Flow
	if err = p.Connection(ctx).
		Where(
			strings.TrimSpace(fmt.Sprintf(`
(state = %d OR state = %d) AND
subject = ? AND
client_id = ? AND
consent_skip=FALSE AND
consent_error='{}' AND
consent_remember=TRUE AND
nid = ?`, flow.FlowStateConsentUsed, flow.FlowStateConsentUnused,
			)),
			subject, client, p.NetworkID(ctx)).
		Order("requested_at DESC").
		Limit(1).
		First(&f); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errorsx.WithStack(consent.ErrNoPreviousConsentFound)
		}
		return nil, sqlcon.HandleError(err)
	}

	return p.filterExpiredConsentRequests(ctx, []flow.AcceptOAuth2ConsentRequest{*f.GetHandledConsentRequest()})
}

func (p *Persister) FindSubjectsGrantedConsentRequests(ctx context.Context, subject string, limit, offset int) ([]flow.AcceptOAuth2ConsentRequest, error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.FindSubjectsGrantedConsentRequests")
	defer span.End()

	var fs []flow.Flow
	c := p.Connection(ctx)

	if err := c.
		Where(
			strings.TrimSpace(fmt.Sprintf(`
(state = %d OR state = %d) AND
subject = ? AND
consent_skip=FALSE AND
consent_error='{}' AND
nid = ?`, flow.FlowStateConsentUsed, flow.FlowStateConsentUnused,
			)),
			subject, p.NetworkID(ctx)).
		Order("requested_at DESC").
		Paginate(offset/limit+1, limit).
		All(&fs); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errorsx.WithStack(consent.ErrNoPreviousConsentFound)
		}
		return nil, sqlcon.HandleError(err)
	}

	var rs []flow.AcceptOAuth2ConsentRequest
	for _, f := range fs {
		rs = append(rs, *f.GetHandledConsentRequest())
	}

	return p.filterExpiredConsentRequests(ctx, rs)
}

func (p *Persister) FindSubjectsSessionGrantedConsentRequests(ctx context.Context, subject, sid string, limit, offset int) ([]flow.AcceptOAuth2ConsentRequest, error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.FindSubjectsSessionGrantedConsentRequests")
	defer span.End()

	var fs []flow.Flow
	c := p.Connection(ctx)

	if err := c.
		Where(
			strings.TrimSpace(fmt.Sprintf(`
(state = %d OR state = %d) AND
subject = ? AND
login_session_id = ? AND
consent_skip=FALSE AND
consent_error='{}' AND
nid = ?`, flow.FlowStateConsentUsed, flow.FlowStateConsentUnused,
			)),
			subject, sid, p.NetworkID(ctx)).
		Order("requested_at DESC").
		Paginate(offset/limit+1, limit).
		All(&fs); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errorsx.WithStack(consent.ErrNoPreviousConsentFound)
		}
		return nil, sqlcon.HandleError(err)
	}

	var rs []flow.AcceptOAuth2ConsentRequest
	for _, f := range fs {
		rs = append(rs, *f.GetHandledConsentRequest())
	}

	return p.filterExpiredConsentRequests(ctx, rs)
}

func (p *Persister) CountSubjectsGrantedConsentRequests(ctx context.Context, subject string) (int, error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.CountSubjectsGrantedConsentRequests")
	defer span.End()

	n, err := p.Connection(ctx).
		Where(
			strings.TrimSpace(fmt.Sprintf(`
(state = %d OR state = %d) AND
subject = ? AND
consent_skip=FALSE AND
consent_error='{}' AND
nid = ?`, flow.FlowStateConsentUsed, flow.FlowStateConsentUnused,
			)),
			subject, p.NetworkID(ctx)).
		Count(&flow.Flow{})
	return n, sqlcon.HandleError(err)
}

func (p *Persister) filterExpiredConsentRequests(ctx context.Context, requests []flow.AcceptOAuth2ConsentRequest) ([]flow.AcceptOAuth2ConsentRequest, error) {
	_, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.filterExpiredConsentRequests")
	defer span.End()

	var result []flow.AcceptOAuth2ConsentRequest
	for _, v := range requests {
		if v.RememberFor > 0 && v.RequestedAt.Add(time.Duration(v.RememberFor)*time.Second).Before(time.Now().UTC()) {
			continue
		}
		result = append(result, v)
	}
	if len(result) == 0 {
		return nil, errorsx.WithStack(consent.ErrNoPreviousConsentFound)
	}
	return result, nil
}

func (p *Persister) ListUserAuthenticatedClientsWithFrontChannelLogout(ctx context.Context, subject, sid string) ([]client.Client, error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.ListUserAuthenticatedClientsWithFrontChannelLogout")
	defer span.End()

	return p.listUserAuthenticatedClients(ctx, subject, sid, "front")
}

func (p *Persister) ListUserAuthenticatedClientsWithBackChannelLogout(ctx context.Context, subject, sid string) ([]client.Client, error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.ListUserAuthenticatedClientsWithBackChannelLogout")
	defer span.End()
	return p.listUserAuthenticatedClients(ctx, subject, sid, "back")
}

func (p *Persister) listUserAuthenticatedClients(ctx context.Context, subject, sid, channel string) ([]client.Client, error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.listUserAuthenticatedClients")
	defer span.End()

	var cs []client.Client
	if err := p.Connection(ctx).RawQuery(
		/* #nosec G201 - channel can either be "front" or "back" */
		fmt.Sprintf(`
SELECT DISTINCT c.* FROM hydra_client as c
JOIN hydra_oauth2_flow as f ON (c.id = f.client_id AND c.nid = f.nid)
WHERE
	f.subject=? AND
	c.%schannel_logout_uri!='' AND
	c.%schannel_logout_uri IS NOT NULL AND
	f.login_session_id = ? AND
	f.nid = ? AND
	c.nid = ?`,
			channel,
			channel,
		),
		subject,
		sid,
		p.NetworkID(ctx),
		p.NetworkID(ctx),
	).All(&cs); err != nil {
		return nil, sqlcon.HandleError(err)
	}

	return cs, nil
}

func (p *Persister) CreateLogoutRequest(ctx context.Context, request *flow.LogoutRequest) error {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.CreateLogoutRequest")
	defer span.End()

	return errorsx.WithStack(p.CreateWithNetwork(ctx, request))
}

func (p *Persister) AcceptLogoutRequest(ctx context.Context, challenge string) (*flow.LogoutRequest, error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.AcceptLogoutRequest")
	defer span.End()

	if err := p.Connection(ctx).RawQuery("UPDATE hydra_oauth2_logout_request SET accepted=true, rejected=false WHERE challenge=? AND nid = ?", challenge, p.NetworkID(ctx)).Exec(); err != nil {
		return nil, sqlcon.HandleError(err)
	}

	return p.GetLogoutRequest(ctx, challenge)
}

func (p *Persister) RejectLogoutRequest(ctx context.Context, challenge string) error {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.RejectLogoutRequest")
	defer span.End()

	count, err := p.Connection(ctx).
		RawQuery("UPDATE hydra_oauth2_logout_request SET rejected=true, accepted=false WHERE challenge=? AND nid = ?", challenge, p.NetworkID(ctx)).
		ExecWithCount()
	if count == 0 {
		return errorsx.WithStack(x.ErrNotFound)
	} else {
		return errorsx.WithStack(err)
	}
}

func (p *Persister) GetLogoutRequest(ctx context.Context, challenge string) (*flow.LogoutRequest, error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.GetLogoutRequest")
	defer span.End()

	var lr flow.LogoutRequest
	return &lr, sqlcon.HandleError(p.QueryWithNetwork(ctx).Where("challenge = ? AND rejected = FALSE", challenge).First(&lr))
}

func (p *Persister) VerifyAndInvalidateLogoutRequest(ctx context.Context, verifier string) (*flow.LogoutRequest, error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.VerifyAndInvalidateLogoutRequest")
	defer span.End()

	var lr flow.LogoutRequest
	if count, err := p.Connection(ctx).RawQuery(
		"UPDATE hydra_oauth2_logout_request SET was_used=TRUE WHERE nid = ? AND verifier=? AND was_used=FALSE AND accepted=TRUE AND rejected=FALSE",
		p.NetworkID(ctx),
		verifier,
	).ExecWithCount(); count == 0 && err == nil {
		return nil, errorsx.WithStack(x.ErrNotFound)
	} else if err != nil {
		return nil, sqlcon.HandleError(err)
	}

	err := sqlcon.HandleError(p.QueryWithNetwork(ctx).Where("verifier=?", verifier).First(&lr))
	if err != nil {
		return nil, err
	}

	return &lr, nil
}

func (p *Persister) FlushInactiveLoginConsentRequests(ctx context.Context, notAfter time.Time, limit int, batchSize int) error {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.FlushInactiveLoginConsentRequests")
	defer span.End()

	/* #nosec G201 table is static */
	var f flow.Flow

	// The value of notAfter should be the minimum between input parameter and request max expire based on its configured age
	requestMaxExpire := time.Now().Add(-p.config.ConsentRequestMaxAge(ctx))
	if requestMaxExpire.Before(notAfter) {
		notAfter = requestMaxExpire
	}

	challenges := []string{}
	queryFormat := `
	SELECT login_challenge
	FROM hydra_oauth2_flow
	WHERE (
		(state != ?)
		OR (login_error IS NOT NULL AND login_error <> '{}' AND login_error <> '')
		OR (consent_error IS NOT NULL AND consent_error <> '{}' AND consent_error <> '')
	)
	AND requested_at < ?
	AND nid = ?
	ORDER BY login_challenge
	LIMIT %[1]d
	`

	// Select up to [limit] flows that can be safely deleted, i.e. flows that meet
	// the following criteria:
	// - flow.state is anything between FlowStateLoginInitialized and FlowStateConsentUnused (unhandled)
	// - flow.login_error has valid error (login rejected)
	// - flow.consent_error has valid error (consent rejected)
	// AND timed-out
	// - flow.requested_at < minimum of ttl.login_consent_request and notAfter
	q := p.Connection(ctx).RawQuery(fmt.Sprintf(queryFormat, limit), flow.FlowStateConsentUsed, notAfter, p.NetworkID(ctx))

	if err := q.All(&challenges); err == sql.ErrNoRows {
		return errors.Wrap(fosite.ErrNotFound, "")
	}

	// Delete in batch consent requests and their references in cascade
	for i := 0; i < len(challenges); i += batchSize {
		j := i + batchSize
		if j > len(challenges) {
			j = len(challenges)
		}

		q := p.Connection(ctx).RawQuery(
			fmt.Sprintf("DELETE FROM %s WHERE login_challenge in (?) AND nid = ?", (&f).TableName()),
			challenges[i:j],
			p.NetworkID(ctx),
		)

		if err := q.Exec(); err != nil {
			return sqlcon.HandleError(err)
		}
	}

	return nil
}
