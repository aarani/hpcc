package logging

import (
	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"
)

// JWTClaims returns a zap field carrying the JWT's claim payload
// decoded WITHOUT signature verification, for forensic logging when a
// token-validation event fires. If the input is empty or malformed
// enough that the payload can't be base64-decoded into a JSON object,
// returns zap.Skip() so the field disappears from the log line rather
// than logging garbage.
//
// SECURITY: the returned claims are unverified. Signature may be
// invalid, expired, or forged; values may be attacker-controlled.
// Treat them as correlation strings only — never make authorization
// decisions from them. To keep that property visible at the log
// destination, the field is always named "jwt_claims_unverified".
//
// We never log the raw token bytes themselves: a valid token is a
// bearer credential, and dumping it into the log pipeline turns the
// log store into a credential store.
func JWTClaims(rawToken string) zap.Field {
	if rawToken == "" {
		return zap.Skip()
	}
	parser := jwt.NewParser()
	token, _, err := parser.ParseUnverified(rawToken, jwt.MapClaims{})
	if err != nil || token == nil {
		return zap.Skip()
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return zap.Skip()
	}
	return zap.Any("jwt_claims_unverified", map[string]any(claims))
}
