package id

import "github.com/google/uuid"

func GenJobID() string {
	return uuid.Must(uuid.NewV7()).String()
}
