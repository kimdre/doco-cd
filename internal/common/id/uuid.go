package id

import "github.com/google/uuid"

func GenID() string {
	return uuid.Must(uuid.NewV7()).String()
}
