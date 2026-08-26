package id

import "uuid"

// New generates a new UUID v7 string.
func New() string {
	return uuid.NewV7().String()
}
