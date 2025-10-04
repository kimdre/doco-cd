package jsonstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"

	"github.com/docker/docker/pkg/jsonmessage"
)

type ctxReader struct {
	err chan error
	r   io.Reader
}

type (
	Stream       = jsonmessage.Stream
	JSONMessage  = jsonmessage.JSONMessage
	JSONError    = jsonmessage.JSONError
	JSONProgress = jsonmessage.JSONProgress
)

var (
	ErrImagePullAccessDenied = errors.New("image pull access denied")
	noSuchImageRegex         = regexp.MustCompile(`No such image:\s*([^\s",]+)`)
	noSuchImageCount         = 0
)

// ErrorReader reads JSON messages from the given reader and returns an error
// if it encounters a JSON error message. It stops reading when the context is done.
func ErrorReader(ctx context.Context, in io.Reader) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	reader := &ctxReader{err: make(chan error, 1), r: in}

	stopFunc := context.AfterFunc(ctx, func() { reader.err <- ctx.Err() })
	defer stopFunc()

	dec := json.NewDecoder(in)

	for {
		var jm JSONMessage
		if err := dec.Decode(&jm); err != nil {
			if err == io.EOF {
				break
			}

			return err
		}

		if jm.Error != nil {
			return errors.New(jm.Error.Message)
		}

		if noSuchImageRegex.MatchString(jm.Status) {
			noSuchImageCount++
			if noSuchImageCount > 3 {
				return fmt.Errorf("%w for '%s', repository does not exist or may require authentication", ErrImagePullAccessDenied, noSuchImageRegex.FindStringSubmatch(jm.Status)[1])
			}
		}
	}

	return ctx.Err()
}
