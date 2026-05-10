package scheduler

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/MicahParks/jwkset"
	"github.com/MicahParks/keyfunc/v3"
	"github.com/aarani/hpcc/internal/protocol/gen"
	"github.com/golang-jwt/jwt/v5"
)

const (
	testWorkerToken = "static-worker-secret-token"
	testIssuer      = "https://idp.test/"
	testAudience    = "scheduler"
	testKID         = "test-kid"
)

func newTestScheduler(t *testing.T) *Scheduler {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate signing keypair: %v", err)
	}
	return &Scheduler{
		config: Config{
			Auth: Auth{
				WorkerToken: testWorkerToken,
				JWKS: JWKSAuth{
					Issuer:   testIssuer,
					Audience: testAudience,
				},
			},
			Routing: Routing{StickyTenants: true},
		},
		signingPubKey:  pub,
		signingPrivKey: priv,
	}
}

// newTestSchedulerWithJWKS returns a scheduler whose jwtKeyFunc validates
// against an in-memory JWKS, plus the IDP private key tests can sign with.
func newTestSchedulerWithJWKS(t *testing.T) (*Scheduler, ed25519.PrivateKey) {
	t.Helper()
	s := newTestScheduler(t)

	idpPub, idpPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate idp keypair: %v", err)
	}

	jwk, err := jwkset.NewJWKFromKey(idpPub, jwkset.JWKOptions{
		Metadata: jwkset.JWKMetadataOptions{KID: testKID},
	})
	if err != nil {
		t.Fatalf("build jwk: %v", err)
	}
	store := jwkset.NewMemoryStorage()
	if err := store.KeyWrite(context.Background(), jwk); err != nil {
		t.Fatalf("store jwk: %v", err)
	}
	raw, err := store.JSON(context.Background())
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	kf, err := keyfunc.NewJWKSetJSON(raw)
	if err != nil {
		t.Fatalf("build keyfunc: %v", err)
	}
	s.jwtKeyFunc = kf
	return s, idpPriv
}

func signIDPJWT(t *testing.T, priv ed25519.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = testKID
	out, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	return out
}

func defaultIDPClaims() jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"iss": testIssuer,
		"aud": testAudience,
		"sub": "user-1",
		"iat": now.Unix(),
		"exp": now.Add(5 * time.Minute).Unix(),
	}
}

// --- Authenticate ---------------------------------------------------------

func TestAuthenticate_StaticToken_Success(t *testing.T) {
	s := newTestScheduler(t)
	resp, err := s.Authenticate(context.Background(), &gen.AuthRequest{
		Token: &gen.AuthRequest_StaticToken{StaticToken: testWorkerToken},
	})
	if err != nil {
		t.Fatalf("Authenticate returned error: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success")
	}
	if resp.SessionToken == "" {
		t.Fatalf("expected non-empty session token")
	}
	if len(resp.SigningPublicKey) != ed25519.PublicKeySize {
		t.Fatalf("expected ed25519 public key (%d bytes), got %d", ed25519.PublicKeySize, len(resp.SigningPublicKey))
	}
	if _, ok := s.workerSessions.Load(resp.SessionToken); !ok {
		t.Fatalf("session token not stored in workerSessions")
	}
}

func TestAuthenticate_StaticToken_Invalid(t *testing.T) {
	s := newTestScheduler(t)
	resp, err := s.Authenticate(context.Background(), &gen.AuthRequest{
		Token: &gen.AuthRequest_StaticToken{StaticToken: "wrong-token"},
	})
	if err != nil {
		t.Fatalf("Authenticate returned error: %v", err)
	}
	if resp.Success {
		t.Fatalf("expected failure for invalid worker token")
	}
	if resp.SessionToken != "" {
		t.Fatalf("expected empty session token on failure")
	}
}

func TestAuthenticate_StaticToken_Empty(t *testing.T) {
	s := newTestScheduler(t)
	resp, _ := s.Authenticate(context.Background(), &gen.AuthRequest{
		Token: &gen.AuthRequest_StaticToken{StaticToken: ""},
	})
	if resp.Success {
		t.Fatalf("empty static token must not authenticate")
	}
}

func TestAuthenticate_NoToken(t *testing.T) {
	s := newTestScheduler(t)
	resp, err := s.Authenticate(context.Background(), &gen.AuthRequest{})
	if err != nil {
		t.Fatalf("Authenticate returned error: %v", err)
	}
	if resp.Success {
		t.Fatalf("expected failure for missing token")
	}
}

func TestAuthenticate_JWT_Success(t *testing.T) {
	s, idpPriv := newTestSchedulerWithJWKS(t)
	tokenStr := signIDPJWT(t, idpPriv, defaultIDPClaims())

	resp, err := s.Authenticate(context.Background(), &gen.AuthRequest{
		Token: &gen.AuthRequest_JwtToken{JwtToken: tokenStr},
	})
	if err != nil {
		t.Fatalf("Authenticate returned error: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected JWT auth success")
	}
	if resp.SessionToken == "" {
		t.Fatalf("expected session token")
	}
	if resp.SigningPublicKey != nil {
		t.Fatalf("user JWT auth must not return signing public key")
	}
	if _, ok := s.userSessions.Load(resp.SessionToken); !ok {
		t.Fatalf("session not stored in userSessions")
	}
}

func TestAuthenticate_JWT_BadIssuer(t *testing.T) {
	s, idpPriv := newTestSchedulerWithJWKS(t)
	claims := defaultIDPClaims()
	claims["iss"] = "https://evil.example/"
	tokenStr := signIDPJWT(t, idpPriv, claims)

	resp, _ := s.Authenticate(context.Background(), &gen.AuthRequest{
		Token: &gen.AuthRequest_JwtToken{JwtToken: tokenStr},
	})
	if resp.Success {
		t.Fatalf("expected JWT with wrong issuer to be rejected")
	}
}

func TestAuthenticate_JWT_BadAudience(t *testing.T) {
	s, idpPriv := newTestSchedulerWithJWKS(t)
	claims := defaultIDPClaims()
	claims["aud"] = "other-service"
	tokenStr := signIDPJWT(t, idpPriv, claims)

	resp, _ := s.Authenticate(context.Background(), &gen.AuthRequest{
		Token: &gen.AuthRequest_JwtToken{JwtToken: tokenStr},
	})
	if resp.Success {
		t.Fatalf("expected JWT with wrong audience to be rejected")
	}
}

func TestAuthenticate_JWT_Expired(t *testing.T) {
	s, idpPriv := newTestSchedulerWithJWKS(t)
	claims := defaultIDPClaims()
	claims["iat"] = time.Now().Add(-1 * time.Hour).Unix()
	claims["exp"] = time.Now().Add(-1 * time.Minute).Unix()
	tokenStr := signIDPJWT(t, idpPriv, claims)

	resp, _ := s.Authenticate(context.Background(), &gen.AuthRequest{
		Token: &gen.AuthRequest_JwtToken{JwtToken: tokenStr},
	})
	if resp.Success {
		t.Fatalf("expected expired JWT to be rejected")
	}
}

func TestAuthenticate_JWT_Garbage(t *testing.T) {
	s, _ := newTestSchedulerWithJWKS(t)
	resp, _ := s.Authenticate(context.Background(), &gen.AuthRequest{
		Token: &gen.AuthRequest_JwtToken{JwtToken: "not-a-jwt"},
	})
	if resp.Success {
		t.Fatalf("expected garbage JWT to be rejected")
	}
}

// --- Route ---------------------------------------------------------------

func TestRoute_Unauthenticated(t *testing.T) {
	s := newTestScheduler(t)
	_, err := s.Route(context.Background(), &gen.RouteRequest{
		SessionToken: "no-such-token",
		TenantId:     "t1",
		ImageDigest:  "img-a",
	})
	if err == nil {
		t.Fatalf("expected unauthenticated error")
	}
}

func TestRoute_NoEligibleWorker(t *testing.T) {
	s := newTestScheduler(t)
	session := "user-session"
	s.userSessions.Store(session, true)

	_, err := s.Route(context.Background(), &gen.RouteRequest{
		SessionToken: session,
		TenantId:     "t1",
		ImageDigest:  "img-missing",
	})
	if err == nil {
		t.Fatalf("expected error when no worker can serve image")
	}
}

func TestRoute_Success(t *testing.T) {
	s := newTestScheduler(t)
	session := "user-session"
	s.userSessions.Store(session, true)

	s.workerStates.Store("w1", &WorkerState{
		WorkerID:        "w1",
		PublicAddr:      "10.0.0.1:9000",
		ImageDigests:    []string{"img-a"},
		AvailableVCPUs:  4,
		CurrentLoad:     1,
		CertFingerprint: []byte{0xAA, 0xBB},
	})

	resp, err := s.Route(context.Background(), &gen.RouteRequest{
		SessionToken: session,
		TenantId:     "t1",
		ImageDigest:  "img-a",
	})
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if resp.WorkerAddress != "10.0.0.1:9000" {
		t.Fatalf("unexpected worker address: %q", resp.WorkerAddress)
	}
	if string(resp.CertFingerprint) != string([]byte{0xAA, 0xBB}) {
		t.Fatalf("unexpected cert fingerprint")
	}

	parsed, err := jwt.Parse(resp.Token, func(*jwt.Token) (any, error) {
		return s.signingPubKey, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}))
	if err != nil || !parsed.Valid {
		t.Fatalf("returned task token did not validate: %v", err)
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		t.Fatalf("expected map claims")
	}
	if claims["tenant_id"] != "t1" || claims["image_digest"] != "img-a" || claims["worker_id"] != "w1" {
		t.Fatalf("unexpected claims: %v", claims)
	}
}

// --- signTaskToken -------------------------------------------------------

func TestSignTaskToken_VerifiesAndExpires(t *testing.T) {
	s := newTestScheduler(t)
	tok, err := s.signTaskToken("tenant-x", "digest-y", "worker-z")
	if err != nil {
		t.Fatalf("signTaskToken: %v", err)
	}

	parsed, err := jwt.Parse(tok, func(*jwt.Token) (any, error) {
		return s.signingPubKey, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}))
	if err != nil || !parsed.Valid {
		t.Fatalf("task token failed to verify: %v", err)
	}

	claims := parsed.Claims.(jwt.MapClaims)
	if claims["tenant_id"] != "tenant-x" {
		t.Fatalf("tenant_id mismatch: %v", claims["tenant_id"])
	}
	if claims["image_digest"] != "digest-y" {
		t.Fatalf("image_digest mismatch: %v", claims["image_digest"])
	}
	if claims["worker_id"] != "worker-z" {
		t.Fatalf("worker_id mismatch: %v", claims["worker_id"])
	}
	exp, _ := claims["exp"].(float64)
	iat, _ := claims["iat"].(float64)
	if int64(exp-iat) != int64(taskTokenTTL.Seconds()) {
		t.Fatalf("exp - iat = %v, want %v seconds", exp-iat, taskTokenTTL.Seconds())
	}

	// Wrong public key must reject the signature.
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := jwt.Parse(tok, func(*jwt.Token) (any, error) {
		return otherPub, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()})); err == nil {
		t.Fatalf("expected verification with wrong key to fail")
	}
}

// --- pickWorker ----------------------------------------------------------

func TestPickWorker_NoWorkers(t *testing.T) {
	s := newTestScheduler(t)
	if _, err := s.pickWorker("t1", "img-a"); err == nil {
		t.Fatalf("expected error with no workers")
	}
}

// TestPickWorker_PicksColdWorker is the converse of the old behavior:
// a worker without the requested image is no longer skipped, just
// penalized. With only one cold worker and free capacity, it should
// still be picked — the worker pulls the image on demand.
func TestPickWorker_PicksColdWorker(t *testing.T) {
	s := newTestScheduler(t)
	s.workerStates.Store("w1", &WorkerState{
		WorkerID: "w1", AvailableVCPUs: 4, ImageDigests: []string{"img-other"},
	})
	got, err := s.pickWorker("t1", "img-a")
	if err != nil {
		t.Fatalf("expected cold worker to be picked, got error: %v", err)
	}
	if got.WorkerID != "w1" {
		t.Fatalf("expected w1, got %q", got.WorkerID)
	}
}

// TestPickWorker_PrefersWarmOverCold confirms the cold-pull penalty
// breaks ties in favor of the warm worker when both have the same
// load.
func TestPickWorker_PrefersWarmOverCold(t *testing.T) {
	s := newTestScheduler(t)
	s.config.Routing.StickyTenants = false
	s.workerStates.Store("cold", &WorkerState{
		WorkerID: "cold", AvailableVCPUs: 4, CurrentLoad: 0,
		ImageDigests: []string{"img-other"},
	})
	s.workerStates.Store("warm", &WorkerState{
		WorkerID: "warm", AvailableVCPUs: 4, CurrentLoad: 0,
		ImageDigests: []string{"img-a"},
	})
	got, err := s.pickWorker("t1", "img-a")
	if err != nil {
		t.Fatalf("pickWorker: %v", err)
	}
	if got.WorkerID != "warm" {
		t.Fatalf("expected warm, got %q", got.WorkerID)
	}
}

func TestPickWorker_SkipsWorkersWithoutVCPUs(t *testing.T) {
	s := newTestScheduler(t)
	s.workerStates.Store("w1", &WorkerState{
		WorkerID: "w1", AvailableVCPUs: 0, ImageDigests: []string{"img-a"},
	})
	if _, err := s.pickWorker("t1", "img-a"); err == nil {
		t.Fatalf("expected error when only worker has no available vcpus")
	}
}

func TestPickWorker_PicksLowestLoad(t *testing.T) {
	s := newTestScheduler(t)
	s.config.Routing.StickyTenants = false

	s.workerStates.Store("busy", &WorkerState{
		WorkerID: "busy", AvailableVCPUs: 4, CurrentLoad: 9,
		ImageDigests: []string{"img-a"},
	})
	s.workerStates.Store("idle", &WorkerState{
		WorkerID: "idle", AvailableVCPUs: 4, CurrentLoad: 1,
		ImageDigests: []string{"img-a"},
	})

	w, err := s.pickWorker("t1", "img-a")
	if err != nil {
		t.Fatalf("pickWorker: %v", err)
	}
	if w.WorkerID != "idle" {
		t.Fatalf("expected idle worker, got %q", w.WorkerID)
	}
}

func TestPickWorker_StickyTenantWins(t *testing.T) {
	s := newTestScheduler(t)
	s.config.Routing.StickyTenants = true

	// "sticky" already has a VM for this tenant+image even though it is
	// more loaded; sticky bonus (-1000) should overpower the load gap.
	s.workerStates.Store("sticky", &WorkerState{
		WorkerID: "sticky", AvailableVCPUs: 4, CurrentLoad: 50,
		ImageDigests: []string{"img-a"},
		ActiveVMs: []VMInfo{{
			TenantID: "t1", ImageDigest: "img-a", VMID: "vm1",
		}},
	})
	s.workerStates.Store("fresh", &WorkerState{
		WorkerID: "fresh", AvailableVCPUs: 4, CurrentLoad: 1,
		ImageDigests: []string{"img-a"},
	})

	w, err := s.pickWorker("t1", "img-a")
	if err != nil {
		t.Fatalf("pickWorker: %v", err)
	}
	if w.WorkerID != "sticky" {
		t.Fatalf("expected sticky worker, got %q", w.WorkerID)
	}
}

func TestPickWorker_StickyDisabled(t *testing.T) {
	s := newTestScheduler(t)
	s.config.Routing.StickyTenants = false

	s.workerStates.Store("sticky", &WorkerState{
		WorkerID: "sticky", AvailableVCPUs: 4, CurrentLoad: 50,
		ImageDigests: []string{"img-a"},
		ActiveVMs: []VMInfo{{
			TenantID: "t1", ImageDigest: "img-a",
		}},
	})
	s.workerStates.Store("fresh", &WorkerState{
		WorkerID: "fresh", AvailableVCPUs: 4, CurrentLoad: 1,
		ImageDigests: []string{"img-a"},
	})

	w, err := s.pickWorker("t1", "img-a")
	if err != nil {
		t.Fatalf("pickWorker: %v", err)
	}
	if w.WorkerID != "fresh" {
		t.Fatalf("with sticky disabled, expected fresh, got %q", w.WorkerID)
	}
}

func TestPickWorker_StickyOnlyForSameTenantImage(t *testing.T) {
	s := newTestScheduler(t)
	s.config.Routing.StickyTenants = true

	// "sticky" hosts a different tenant — should NOT receive the sticky bonus.
	s.workerStates.Store("sticky", &WorkerState{
		WorkerID: "sticky", AvailableVCPUs: 4, CurrentLoad: 50,
		ImageDigests: []string{"img-a"},
		ActiveVMs: []VMInfo{{
			TenantID: "other-tenant", ImageDigest: "img-a",
		}},
	})
	s.workerStates.Store("fresh", &WorkerState{
		WorkerID: "fresh", AvailableVCPUs: 4, CurrentLoad: 1,
		ImageDigests: []string{"img-a"},
	})

	w, err := s.pickWorker("t1", "img-a")
	if err != nil {
		t.Fatalf("pickWorker: %v", err)
	}
	if w.WorkerID != "fresh" {
		t.Fatalf("expected fresh (no sticky match for tenant), got %q", w.WorkerID)
	}
}

// --- RegisterWorker ------------------------------------------------------

func TestRegisterWorker_Unauthenticated(t *testing.T) {
	s := newTestScheduler(t)
	resp, err := s.RegisterWorker(context.Background(), &gen.RegisterWorkerRequest{
		SessionToken: "unknown",
		WorkerId:     "w1",
	})
	if err != nil {
		t.Fatalf("RegisterWorker returned error: %v", err)
	}
	if resp.Success {
		t.Fatalf("expected failure for unauthenticated session")
	}
}

func TestRegisterWorker_Success(t *testing.T) {
	s := newTestScheduler(t)
	session := "worker-session"
	s.workerSessions.Store(session, true)

	resp, err := s.RegisterWorker(context.Background(), &gen.RegisterWorkerRequest{
		SessionToken:    session,
		WorkerId:        "w1",
		PublicAddr:      "1.2.3.4:9000",
		ImageDigests:    []string{"img-a", "img-b"},
		AvailableVcpus:  8,
		CurrentLoad:     2,
		CertFingerprint: []byte{0x01, 0x02},
	})
	if err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected RegisterWorker success")
	}

	// Session now mapped to the worker ID, not just `true`.
	v, ok := s.workerSessions.Load(session)
	if !ok {
		t.Fatalf("session lost after RegisterWorker")
	}
	if id, _ := v.(string); id != "w1" {
		t.Fatalf("session should map to worker id, got %v", v)
	}

	// Worker state stored.
	stateAny, ok := s.workerStates.Load("w1")
	if !ok {
		t.Fatalf("worker state not stored")
	}
	state := stateAny.(*WorkerState)
	if state.PublicAddr != "1.2.3.4:9000" || state.AvailableVCPUs != 8 {
		t.Fatalf("worker state not populated: %+v", state)
	}
}

// --- Heartbeat -----------------------------------------------------------

func TestHeartbeat_Unauthenticated(t *testing.T) {
	s := newTestScheduler(t)
	_, err := s.Heartbeat(context.Background(), &gen.WorkerHeartbeat{
		SessionToken: "unknown", WorkerId: "w1",
	})
	if err == nil || !strings.Contains(err.Error(), "unauthenticated") {
		t.Fatalf("expected unauthenticated error, got %v", err)
	}
}

func TestHeartbeat_WorkerIDMismatch(t *testing.T) {
	s := newTestScheduler(t)
	session := "session-1"
	s.workerSessions.Store(session, "registered-worker")

	_, err := s.Heartbeat(context.Background(), &gen.WorkerHeartbeat{
		SessionToken: session, WorkerId: "different-worker",
	})
	if err == nil || !strings.Contains(err.Error(), "session does not match") {
		t.Fatalf("expected session/worker mismatch error, got %v", err)
	}
}

func TestHeartbeat_WorkerNotRegistered(t *testing.T) {
	s := newTestScheduler(t)
	session := "session-1"
	s.workerSessions.Store(session, "w1")
	// no workerStates entry

	_, err := s.Heartbeat(context.Background(), &gen.WorkerHeartbeat{
		SessionToken: session, WorkerId: "w1",
	})
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("expected not-registered error, got %v", err)
	}
}

func TestHeartbeat_UpdatesState(t *testing.T) {
	s := newTestScheduler(t)
	session := "session-1"
	s.workerSessions.Store(session, "w1")
	s.workerStates.Store("w1", &WorkerState{
		WorkerID:       "w1",
		AvailableVCPUs: 4,
		CurrentLoad:    1,
	})

	_, err := s.Heartbeat(context.Background(), &gen.WorkerHeartbeat{
		SessionToken:   session,
		WorkerId:       "w1",
		AvailableVcpus: 7,
		CurrentLoad:    3,
		ActiveVms: []*gen.ActiveVM{{
			VmId: "vm-1", TenantId: "t1", ImageDigest: "img-a",
		}},
	})
	if err != nil {
		t.Fatalf("Heartbeat returned error: %v", err)
	}

	stateAny, _ := s.workerStates.Load("w1")
	state := stateAny.(*WorkerState)
	if state.AvailableVCPUs != 7 || state.CurrentLoad != 3 {
		t.Fatalf("heartbeat did not update vCPUs/load: %+v", state)
	}
	if len(state.ActiveVMs) != 1 || state.ActiveVMs[0].VMID != "vm-1" {
		t.Fatalf("heartbeat did not record active VMs: %+v", state.ActiveVMs)
	}
	if state.LastHeartbeat.IsZero() {
		t.Fatalf("LastHeartbeat not set")
	}
}

// Confirm the auth → register → heartbeat happy path can be threaded with
// the same session token end-to-end.
func TestEndToEndWorkerLifecycle(t *testing.T) {
	s := newTestScheduler(t)

	authResp, err := s.Authenticate(context.Background(), &gen.AuthRequest{
		Token: &gen.AuthRequest_StaticToken{StaticToken: testWorkerToken},
	})
	if err != nil || !authResp.Success {
		t.Fatalf("worker authenticate failed: %v / %+v", err, authResp)
	}

	regResp, err := s.RegisterWorker(context.Background(), &gen.RegisterWorkerRequest{
		SessionToken:   authResp.SessionToken,
		WorkerId:       "w1",
		PublicAddr:     "addr",
		ImageDigests:   []string{"img-a"},
		AvailableVcpus: 4,
	})
	if err != nil || !regResp.Success {
		t.Fatalf("RegisterWorker failed: %v / %+v", err, regResp)
	}

	if _, err := s.Heartbeat(context.Background(), &gen.WorkerHeartbeat{
		SessionToken:   authResp.SessionToken,
		WorkerId:       "w1",
		AvailableVcpus: 4,
		CurrentLoad:    1,
	}); err != nil {
		t.Fatalf("Heartbeat failed: %v", err)
	}
}
