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
	Email      string `json:"email"`
	MerchantID string `json:"merchant_id"`
	Role       string `json:"role"`
	jwt.RegisteredClaims
}

func IssueToken(secret, email, role string, staffID, merchantID uuid.UUID, ttl time.Duration) (string, time.Time, error) {
	exp := time.Now().Add(ttl)
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		Email:      email,
		MerchantID: merchantID.String(),
		Role:       role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   staffID.String(),
			Issuer:    "till",
			ExpiresAt: jwt.NewNumericDate(exp),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	})
	s, err := t.SignedString([]byte(secret))
	return s, exp, err
}

func ParseToken(secret, token string) (staffID, merchantID uuid.UUID, email, role string, err error) {
	parsed, err := jwt.ParseWithClaims(token, &Claims{}, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return []byte(secret), nil
	})
	if err != nil {
		return uuid.Nil, uuid.Nil, "", "", err
	}
	claims, ok := parsed.Claims.(*Claims)
	if !ok || !parsed.Valid {
		return uuid.Nil, uuid.Nil, "", "", fmt.Errorf("invalid token")
	}
	staffID, err = uuid.Parse(claims.Subject)
	if err != nil {
		return uuid.Nil, uuid.Nil, "", "", err
	}
	merchantID, err = uuid.Parse(claims.MerchantID)
	if err != nil {
		return uuid.Nil, uuid.Nil, "", "", err
	}
	return staffID, merchantID, claims.Email, claims.Role, nil
}
