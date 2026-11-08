package auth

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	RoleAdmin   = "admin"
	RoleManager = "manager"
	RoleCashier = "cashier"
)

func ValidRole(role string) bool {
	switch role {
	case RoleAdmin, RoleManager, RoleCashier:
		return true
	}
	return false
}

type Claims struct {
	Email        string `json:"email"`
	MerchantID   string `json:"merchant_id"`
	Role         string `json:"role"`
	TokenVersion int    `json:"tv"`
	jwt.RegisteredClaims
}

// Identity is what a verified token says about the caller.
type Identity struct {
	StaffID      uuid.UUID
	MerchantID   uuid.UUID
	Email        string
	Role         string
	TokenVersion int
	JTI          string
}

// IssueToken signs a token that carries the staff row's token_version.
// Bumping token_version on the staff row revokes every token issued before.
func IssueToken(secret, email, role string, staffID, merchantID uuid.UUID, tokenVersion int, ttl time.Duration) (string, time.Time, error) {
	now := time.Now()
	exp := now.Add(ttl)
	jti, err := uuid.NewV7()
	if err != nil {
		return "", time.Time{}, err
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		Email:        email,
		MerchantID:   merchantID.String(),
		Role:         role,
		TokenVersion: tokenVersion,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        jti.String(),
			Subject:   staffID.String(),
			Issuer:    "till",
			ExpiresAt: jwt.NewNumericDate(exp),
			IssuedAt:  jwt.NewNumericDate(now),
		},
	})
	s, err := t.SignedString([]byte(secret))
	return s, exp, err
}

func ParseToken(secret, token string) (Identity, error) {
	parsed, err := jwt.ParseWithClaims(token, &Claims{}, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return []byte(secret), nil
	}, jwt.WithIssuer("till"), jwt.WithExpirationRequired())
	if err != nil {
		return Identity{}, err
	}
	claims, ok := parsed.Claims.(*Claims)
	if !ok || !parsed.Valid {
		return Identity{}, fmt.Errorf("invalid token")
	}
	staffID, err := uuid.Parse(claims.Subject)
	if err != nil {
		return Identity{}, err
	}
	merchantID, err := uuid.Parse(claims.MerchantID)
	if err != nil {
		return Identity{}, err
	}
	return Identity{
		StaffID: staffID, MerchantID: merchantID, Email: claims.Email, Role: claims.Role,
		TokenVersion: claims.TokenVersion, JTI: claims.ID,
	}, nil
}
