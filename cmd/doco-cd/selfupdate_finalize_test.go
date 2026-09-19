package main

import (
	"testing"

	"github.com/kimdre/doco-cd/internal/selfupdate"
)

func TestResolveSelfUpdateRole(t *testing.T) {
	t.Parallel()

	record := selfupdate.Record{
		Predecessor: selfupdate.ContainerRef{ID: "old"},
		Successor:   selfupdate.ContainerRef{ID: "new"},
	}

	restored := selfupdate.Record{
		Predecessor: selfupdate.ContainerRef{ID: "old"},
		Restored:    selfupdate.ContainerRef{ID: "restored"},
	}

	tests := []struct {
		name   string
		record selfupdate.Record
		ownID  string
		want   selfUpdateRole
	}{
		{name: "predecessor", record: record, ownID: "old", want: rolePredecessor},
		{name: "successor", record: record, ownID: "new", want: roleSuccessor},
		{name: "restored predecessor", record: restored, ownID: "restored", want: rolePredecessor},
		// The applier creates the successor, so the record does not name it.
		{name: "unnamed successor", record: record, ownID: "created-by-the-applier", want: roleSuccessor},
		{name: "no container id", record: record, want: roleUnknown},
		{name: "record without a predecessor", record: selfupdate.Record{}, ownID: "whatever", want: roleUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := resolveSelfUpdateRole(tt.record, tt.ownID); got != tt.want {
				t.Errorf("role = %q, want %q", got, tt.want)
			}
		})
	}
}
