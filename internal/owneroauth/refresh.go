package owneroauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"authelia.com/provider/oauth2"
	grantflow "authelia.com/provider/oauth2/handler/oauth2"
	bolt "go.etcd.io/bbolt"
)

type tokenPair struct {
	Access    string `json:"access_token"`
	Refresh   string `json:"refresh_token,omitempty"`
	Type      string `json:"token_type"`
	ExpiresIn int64  `json:"expires_in"`
}

// Pairing assigns a fresh client ID for each approval. Its bucket contains the
// current pair's hashes and one hash key for each consumed refresh token.
type refreshFamily struct {
	Resource        string
	Access, Refresh []byte
	Expires         time.Time
}

func pruneFamilies(tx *bolt.Tx, now time.Time) error {
	families := tx.Bucket([]byte("families"))
	if families == nil {
		return errState
	}
	c := families.Cursor()
	for k, _ := c.First(); k != nil; {
		var f refreshFamily
		bucket := families.Bucket(k)
		if bucket == nil {
			return errState
		}
		if err := json.Unmarshal(bucket.Get([]byte("current")), &f); err != nil {
			return errState
		}
		if now.Before(f.Expires) {
			k, _ = c.Next()
			continue
		}

		if err := families.DeleteBucket(k); err != nil {
			return err
		}
		k, _ = c.Seek(k)
	}
	return nil
}

// Both phases of the provider's refresh grant use this one bbolt writer.
// No other exchange can observe rotation or revocation before it commits.
type refreshTx struct {
	tx     *bolt.Tx
	bucket *bolt.Bucket
	family refreshFamily
	client *oauth2.DefaultClient
	now    time.Time
}

func (a *Authority) refresh(ctx context.Context, form url.Values) (tokenPair, error) {
	client, resource := form.Get("client_id"), a.issuer+"/mcp"
	if client == "" || form.Get("refresh_token") == "" || (form.Has("resource") && form.Get("resource") != resource) {
		return tokenPair{}, errDenied
	}

	var pair tokenPair
	err := a.db.Update(func(tx *bolt.Tx) error {
		families := tx.Bucket([]byte("families"))
		if families == nil {
			return errState
		}
		key := digest(client)
		bucket := families.Bucket(key)
		if bucket == nil {
			return errDenied
		}

		s := refreshTx{
			tx: tx, bucket: bucket, now: a.settings.Now(),
			client: &oauth2.DefaultClient{
				ID: client, Public: true, GrantTypes: []string{"refresh_token"}, Resource: []string{resource},
			},
		}
		if err := json.Unmarshal(bucket.Get([]byte("current")), &s.family); err != nil {
			return errState
		}
		if s.family.Resource != resource {
			return errDenied
		}
		if !s.now.Before(s.family.Expires) {
			return families.DeleteBucket(key)
		}

		req := oauth2.NewAccessRequest(&oauth2.DefaultSession{})
		req.Client = s.client
		req.GrantTypes = oauth2.Arguments{"refresh_token"}
		req.Form = form
		req.SetRequestedScopes(strings.Fields(form.Get("scope")))
		req.SetRequestedResource(oauth2.Arguments{resource})
		req.SetRequestedAudience(strings.Fields(form.Get("audience")))

		handler := grantflow.RefreshTokenGrantHandler{
			Config: &oauth2.Config{
				AccessTokenLifespan: a.settings.AccessLifetime, RefreshTokenLifespan: a.settings.RefreshIdleTimeout,
				RefreshTokenScopes: []string{}, ResourceStrategy: oauth2.ExactResourceStrategy,
			},
			TokenRevocationStorage: &s, RefreshTokenStrategy: &s, AccessTokenStrategy: &s,
		}
		if err := handler.HandleTokenEndpointRequest(ctx, req); err != nil {
			switch {
			case errors.Is(err, oauth2.ErrServerError):
				return errState
			case errors.Is(err, oauth2.ErrInvalidGrant) && families.Bucket(key) == nil:
				// Replay is an invalid grant *after* the handler revokes the family.
				// Commit that revocation and report the denial outside Update.
				return nil
			case errors.Is(err, oauth2.ErrInvalidScope):
				return oauth2.ErrInvalidScope
			default:
				return errDenied
			}
		}

		// Use the authority's clock for durable deadlines as on code exchange.
		req.GetSession().SetExpiresAt(oauth2.AccessToken, s.now.Add(a.settings.AccessLifetime))
		req.GetSession().SetExpiresAt(oauth2.RefreshToken, s.now.Add(a.settings.RefreshIdleTimeout))
		response := oauth2.NewAccessResponse()
		if err := handler.PopulateTokenEndpointResponse(ctx, req, response); err != nil {
			return errState
		}

		refresh, _ := response.GetExtra("refresh_token").(string)
		if response.GetAccessToken() == "" || refresh == "" {
			return errState
		}

		pair = tokenPair{Access: response.GetAccessToken(), Refresh: refresh}
		return nil
	})
	if errors.Is(err, oauth2.ErrInvalidScope) {
		return tokenPair{}, oauth2.ErrInvalidScope
	}
	if err != nil && !errors.Is(err, errDenied) {
		return tokenPair{}, errState
	}
	if pair.Access == "" {
		return tokenPair{}, errDenied
	}

	return pair, nil
}

func (s *refreshTx) GetRefreshTokenSession(_ context.Context, signature string, _ oauth2.Session) (oauth2.Requester, error) {
	key := []byte(signature)
	current := bytes.Equal(key, s.family.Refresh)
	if !current && s.bucket.Get(key) == nil {
		return nil, oauth2.ErrNotFound
	}

	req := oauth2.NewRequest()
	req.ID = s.client.ID
	req.Client = s.client
	req.GrantResource(s.family.Resource)
	req.Session = &oauth2.DefaultSession{}
	req.Session.SetExpiresAt(oauth2.RefreshToken, s.family.Expires)
	if !current {
		return req, oauth2.ErrInactiveToken
	}

	return req, nil
}

func (s *refreshTx) RotateRefreshToken(ctx context.Context, id, signature string) error {
	if _, err := s.GetRefreshTokenSession(ctx, signature, nil); err != nil {
		return err
	}
	if err := s.bucket.Put([]byte(signature), []byte{}); err != nil {
		return err
	}

	s.family.Refresh = nil
	return s.RevokeAccessToken(ctx, id)
}

func (s *refreshTx) CreateAccessTokenSession(_ context.Context, signature string, req oauth2.Requester) error {
	return putRecord(s.tx.Bucket([]byte("access")), []byte(signature), access{s.family.Resource, req.GetSession().GetExpiresAt(oauth2.AccessToken)})
}

func (s *refreshTx) CreateRefreshTokenSession(_ context.Context, signature, accessSignature string, req oauth2.Requester) error {
	s.family.Refresh = []byte(signature)
	s.family.Access = []byte(accessSignature)
	s.family.Expires = req.GetSession().GetExpiresAt(oauth2.RefreshToken)

	return putRecord(s.bucket, []byte("current"), s.family)
}

func (s *refreshTx) RevokeRefreshToken(_ context.Context, id string) error {
	return s.tx.Bucket([]byte("families")).DeleteBucket(digest(id))
}

func (s *refreshTx) RevokeAccessToken(_ context.Context, _ string) error {
	b := s.tx.Bucket([]byte("access"))
	if b == nil {
		return errState
	}

	return b.Delete(s.family.Access)
}

// The rotating grant never updates a refresh token in place, deletes a single
// refresh session, or looks up an access token for introspection.
func (*refreshTx) UpdateRefreshTokenSession(context.Context, string, oauth2.Requester) error {
	return errors.ErrUnsupported
}

func (*refreshTx) DeleteRefreshTokenSession(context.Context, string) error {
	return errors.ErrUnsupported
}

func (*refreshTx) GetAccessTokenSession(context.Context, string, oauth2.Session) (oauth2.Requester, error) {
	return nil, errors.ErrUnsupported
}

func (*refreshTx) DeleteAccessTokenSession(context.Context, string) error {
	return errors.ErrUnsupported
}

// The provider calls the strategy with the same opaque tokens used on code
// exchange. Only their SHA-256 hashes serve as storage signatures.
func (*refreshTx) GenerateAccessToken(context.Context, oauth2.Requester) (string, string, error) {
	token := secret()
	return token, string(digest(token)), nil
}

func (s *refreshTx) GenerateRefreshToken(ctx context.Context, req oauth2.Requester) (string, string, error) {
	return s.GenerateAccessToken(ctx, req)
}

func (*refreshTx) AccessTokenSignature(_ context.Context, token string) string {
	return string(digest(token))
}

func (*refreshTx) RefreshTokenSignature(_ context.Context, token string) string {
	return string(digest(token))
}

// Neither opaque token type carries a distinguishing prefix.
func (*refreshTx) IsOpaqueAccessToken(context.Context, string) bool  { return false }
func (*refreshTx) IsOpaqueRefreshToken(context.Context, string) bool { return false }

func (*refreshTx) ValidateAccessToken(context.Context, oauth2.Requester, string) error {
	return errors.ErrUnsupported
}

func (s *refreshTx) ValidateRefreshToken(_ context.Context, _ oauth2.Requester, token string) error {
	key := digest(token)
	if !bytes.Equal(key, s.family.Refresh) && s.bucket.Get(key) == nil {
		return oauth2.ErrTokenSignatureMismatch
	}
	if !s.now.Before(s.family.Expires) {
		return oauth2.ErrTokenExpired
	}

	return nil
}
