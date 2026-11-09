// Package model contains the data structures shared by the storage layer,
// the HTTP API and the web UI.
package model

type User struct {
	ID           string
	Email        string
	PasswordHash string
	Role         string
	Status       string
	QuotaBytes   int64
	UsedBytes    int64
	CreatedAt    int64
	UpdatedAt    int64
}

func (u User) IsAdmin() bool  { return u.Role == "admin" }
func (u User) IsActive() bool { return u.Status == "active" }

type Session struct {
	ID         string
	UserID     string
	TokenHash  string
	IP         string
	UserAgent  string
	CreatedAt  int64
	LastSeenAt int64
	ExpiresAt  int64
}

type APIToken struct {
	ID          string
	UserID      string
	Name        string
	TokenHash   string
	TokenPrefix string
	Scopes      string
	CreatedAt   int64
	LastUsedAt  *int64
	ExpiresAt   *int64
}

type Image struct {
	ID           string
	UserID       string
	ObjectKey    string
	OriginalName string
	MIME         string
	SizeBytes    int64
	Width        int
	Height       int
	SHA256       string
	Visibility   string
	CreatedAt    int64
	DeletedAt    *int64
}

type Invite struct {
	Code      string
	CreatedBy *string
	UsedBy    *string
	CreatedAt int64
	ExpiresAt *int64
}

type AuditLog struct {
	ID        string
	ActorID   *string
	Action    string
	Target    string
	IP        string
	Meta      string
	CreatedAt int64
}

type CDNPurge struct {
	URL       string
	CreatedAt int64
	Attempts  int
	LastError string
}
