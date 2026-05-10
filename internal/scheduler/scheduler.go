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
	jwtKeyFunc     keyfunc.Keyfunc
	workerSessions sync.Map // session token → worker ID
	userSessions   sync.Map // session token → true
	workerStates   sync.Map // worker ID → *WorkerState
	signingPrivKey ed25519.PrivateKey
	signingPubKey  ed25519.PublicKey
	gen.UnimplementedSchedulerServiceServer
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

	kf, err := keyfunc.NewDefault([]string{config.Auth.JWKS.URL})
	if err != nil {
		return nil, fmt.Errorf("init JWKS keyfunc from %q: %w", config.Auth.JWKS.URL, err)
	}

	return &Scheduler{
		config:         config,
		jwtKeyFunc:     kf,
		signingPrivKey: privKey,
		signingPubKey:  pubKey,
	}, nil
}

var _ gen.SchedulerServiceServer = (*Scheduler)(nil)

func (s *Scheduler) Authenticate(ctx context.Context, in *gen.AuthRequest) (*gen.AuthResponse, error) {
	switch t := in.GetToken().(type) {
	case *gen.AuthRequest_JwtToken:
		token, err := jwt.Parse(t.JwtToken, s.jwtKeyFunc.Keyfunc,
			jwt.WithIssuer(s.config.Auth.JWKS.Issuer),
			jwt.WithAudience(s.config.Auth.JWKS.Audience),
		)
		if err != nil || !token.Valid {
			return &gen.AuthResponse{Success: false}, nil
		}

		sessionToken := rand.Text()
		s.userSessions.Store(sessionToken, true)
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
	if _, authenticated := s.userSessions.Load(in.SessionToken); !authenticated {
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
