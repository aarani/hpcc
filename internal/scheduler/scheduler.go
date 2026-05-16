package scheduler

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/aarani/hpcc/internal/protocol/gen"
	"github.com/golang-jwt/jwt/v5"
)

type Scheduler struct {
	config         Config
	tenantAuth     map[string]*tenantAuth // tenant_id → per-tenant IdP
	workerSessions sync.Map               // session token → worker ID
	userSessions   sync.Map               // session token → true
	workerStates   sync.Map               // worker ID → *WorkerState
	signingPrivKey ed25519.PrivateKey
	signingPubKey  ed25519.PublicKey
	gen.UnimplementedSchedulerServiceServer
}

// tenantAuth bundles the JWKS keyfunc and expected iss/aud for a single
// tenant. Issuer/audience are checked manually rather than via
// jwt.WithIssuer/jwt.WithAudience because the parser's keyfunc fires
// before those validators, and we need to choose the keyfunc based on
// the tenant claim — so we run all post-signature checks ourselves
// after picking the tenant.
type tenantAuth struct {
	keyfunc  keyfunc.Keyfunc
	issuer   string
	tokenURL string
	audience string
	clientID string
	scope    string
}

func NewDefaultScheduler() (*Scheduler, error) {
	path, err := DefaultConfigPath()

	if err != nil {
		return nil, err
	}

	cfg, err := LoadConfig(path)

	if err != nil {
		return nil, err
	}

	return NewScheduler(cfg)
}

func NewScheduler(config Config) (*Scheduler, error) {
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate signing keypair: %w", err)
	}

	tenants := make(map[string]*tenantAuth, len(config.Tenants))
	for _, t := range config.Tenants {
		kf, err := keyfunc.NewDefault([]string{t.JWKSURL})
		if err != nil {
			return nil, fmt.Errorf("init JWKS keyfunc for tenant %q from %q: %w", t.ID, t.JWKSURL, err)
		}
		tenants[t.ID] = &tenantAuth{
			keyfunc:  kf,
			issuer:   t.Issuer,
			tokenURL: t.TokenURL,
			audience: t.Audience,
			clientID: t.ClientID,
			scope:    t.Scope,
		}
	}

	return &Scheduler{
		config:         config,
		tenantAuth:     tenants,
		signingPrivKey: privKey,
		signingPubKey:  pubKey,
	}, nil
}

var _ gen.SchedulerServiceServer = (*Scheduler)(nil)

// GetTenantIdP returns the OAuth discovery info for a tenant so the
// client doesn't have to hardcode token_url/issuer/audience in its
// config. Unauthenticated by design — the client has only its
// tenant_id + scheduler URL at this point. Unknown tenant returns an
// error; the response contains no secrets (issuer/token_url are
// publicly observable in any issued JWT or OAuth flow).
func (s *Scheduler) GetTenantIdP(ctx context.Context, in *gen.GetTenantIdPRequest) (*gen.GetTenantIdPResponse, error) {
	ta, ok := s.tenantAuth[in.TenantId]
	if !ok {
		return nil, fmt.Errorf("unknown tenant")
	}
	return &gen.GetTenantIdPResponse{
		Issuer:   ta.issuer,
		TokenUrl: ta.tokenURL,
		Audience: ta.audience,
		ClientId: ta.clientID,
		Scope:    ta.scope,
	}, nil
}

func (s *Scheduler) Authenticate(ctx context.Context, in *gen.AuthRequest) (*gen.AuthResponse, error) {
	switch t := in.GetToken().(type) {
	case *gen.AuthRequest_JwtToken:
		if !s.verifyTenantJWT(in.TenantId, t.JwtToken) {
			// Single opaque rejection across "missing tenant_id",
			// "unknown tenant", "bad signature", "bad iss/aud",
			// and "expired" — distinguishing any of these would
			// leak tenant enumeration. See docs/plan/multi-tenant.md.
			return &gen.AuthResponse{Success: false}, nil
		}

		sessionToken := rand.Text()
		// Bind the session to the tenant the JWT was validated for so
		// downstream Route calls can reject session_token-for-A used
		// against tenant_id=B.
		s.userSessions.Store(sessionToken, in.TenantId)
		return &gen.AuthResponse{Success: true, SessionToken: sessionToken}, nil

	case *gen.AuthRequest_StaticToken:
		tokenBytes := []byte(t.StaticToken)
		expectedBytes := []byte(s.config.Auth.WorkerToken)
		if subtle.ConstantTimeCompare(tokenBytes, expectedBytes) != 1 {
			return &gen.AuthResponse{Success: false}, nil
		}

		sessionToken := rand.Text()
		s.workerSessions.Store(sessionToken, true)
		return &gen.AuthResponse{
			Success:          true,
			SessionToken:     sessionToken,
			SigningPublicKey: s.signingPubKey,
		}, nil

	default:
		return &gen.AuthResponse{Success: false}, nil
	}
}

func (s *Scheduler) Route(ctx context.Context, in *gen.RouteRequest) (*gen.RouteResponse, error) {
	sessVal, authenticated := s.userSessions.Load(in.SessionToken)
	if !authenticated {
		return nil, fmt.Errorf("unauthenticated")
	}
	sessTenant, _ := sessVal.(string)
	// Session is bound to the tenant the JWT authenticated. A session
	// for tenant A cannot route as tenant B even if the JWT verified.
	if sessTenant == "" || sessTenant != in.TenantId {
		return nil, fmt.Errorf("unauthenticated")
	}

	worker, err := s.pickWorker(in.TenantId, in.ImageDigest)
	if err != nil {
		return nil, err
	}

	taskToken, err := s.signTaskToken(in.TenantId, in.ImageDigest, worker.WorkerID)
	if err != nil {
		return nil, fmt.Errorf("sign task token: %w", err)
	}

	return &gen.RouteResponse{
		WorkerAddress:   worker.PublicAddr,
		Token:           taskToken,
		CertFingerprint: worker.CertFingerprint,
	}, nil
}

// verifyTenantJWT runs the per-tenant validation chain described in
// docs/plan/multi-tenant.md: tenants[tenantID] → signature against that
// tenant's JWKS → iss/aud match. tenantID comes from the
// AuthRequest, not from a JWT claim — this is the property that
// stops an IdP configured for tenant A from ever being asked to
// validate something the client labeled as tenant B. The caller
// MUST collapse all failure modes into one opaque rejection.
func (s *Scheduler) verifyTenantJWT(tenantID, raw string) bool {
	if tenantID == "" {
		return false
	}
	ta, ok := s.tenantAuth[tenantID]
	if !ok {
		return false
	}

	token, err := jwt.Parse(raw, ta.keyfunc.Keyfunc)
	if err != nil || !token.Valid {
		return false
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return false
	}
	if iss, _ := claims["iss"].(string); iss != ta.issuer {
		return false
	}
	// aud may be string or []string per RFC 7519. jwt/v5 surfaces it
	// as either; check both shapes.
	if !audienceMatches(claims["aud"], ta.audience) {
		return false
	}
	return true
}

func audienceMatches(claim any, want string) bool {
	switch v := claim.(type) {
	case string:
		return v == want
	case []any:
		for _, a := range v {
			if s, ok := a.(string); ok && s == want {
				return true
			}
		}
	case []string:
		for _, s := range v {
			if s == want {
				return true
			}
		}
	}
	return false
}

const taskTokenTTL = 5 * time.Minute

func (s *Scheduler) signTaskToken(tenantID, imageDigest, workerID string) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"tenant_id":    tenantID,
		"image_digest": imageDigest,
		"worker_id":    workerID,
		"iat":          now.Unix(),
		"exp":          now.Add(taskTokenTTL).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	return token.SignedString(s.signingPrivKey)
}

// coldPullPenalty is added to a worker's routing score when it doesn't
// already have the requested image. It's enough to lose against any
// warm worker at comparable load, but small enough that an unloaded
// cold worker still wins over a saturated warm one.
const coldPullPenalty = 100

func (s *Scheduler) pickWorker(tenantID, imageDigest string) (*WorkerState, error) {
	var best *WorkerState
	bestScore := math.MaxFloat64

	s.workerStates.Range(func(_, val any) bool {
		w := val.(*WorkerState)

		if w.AvailableVCPUs <= 0 {
			return true
		}

		score := float64(w.CurrentLoad)

		hasImage := false
		for _, d := range w.ImageDigests {
			if d == imageDigest {
				hasImage = true
				break
			}
		}
		if !hasImage {
			// Worker can pull on demand — penalize, don't exclude.
			score += coldPullPenalty
		}

		if s.config.Routing.StickyTenants {
			for _, vm := range w.ActiveVMs {
				if vm.TenantID == tenantID && vm.ImageDigest == imageDigest {
					score -= 1000
					break
				}
			}
		}

		if score < bestScore {
			bestScore = score
			best = w
		}
		return true
	})

	if best == nil {
		return nil, fmt.Errorf("no available worker (no registered worker has free capacity)")
	}
	return best, nil
}

func (s *Scheduler) RegisterWorker(ctx context.Context, in *gen.RegisterWorkerRequest) (*gen.RegisterWorkerResponse, error) {
	if _, authenticated := s.workerSessions.Load(in.SessionToken); !authenticated {
		return &gen.RegisterWorkerResponse{Success: false}, nil
	}

	s.workerSessions.Store(in.SessionToken, in.WorkerId)

	state := workerStateFromRegistration(in)
	s.workerStates.Store(in.WorkerId, state)
	return &gen.RegisterWorkerResponse{Success: true}, nil
}

func (s *Scheduler) Heartbeat(ctx context.Context, in *gen.WorkerHeartbeat) (*gen.HeartbeatResponse, error) {
	workerID, authenticated := s.workerSessions.Load(in.SessionToken)
	if !authenticated {
		return nil, fmt.Errorf("unauthenticated")
	}

	if workerID != in.WorkerId {
		return nil, fmt.Errorf("session does not match worker ID")
	}

	val, exists := s.workerStates.Load(in.WorkerId)
	if !exists {
		return nil, fmt.Errorf("worker %q not registered", in.WorkerId)
	}

	state := val.(*WorkerState)
	state.applyHeartbeat(in)
	return &gen.HeartbeatResponse{}, nil
}
