package upload

import "fujipic/internal/id"

func randomKey() string  { return id.RandomBase32(16) }
func newImageID() string { return id.New("img") }
func newAuditID() string { return id.New("aud") }
