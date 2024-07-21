package compose

import (
	"errors"
	"net"
	"os"
)

const (
	socketNetwork = "unix"
	socketAddress = "/var/run/docker.sock"
)

var ErrDockerSocketConnectionFailed = errors.New("failed to connect to docker socket")

// ConnectToSocket connects to the docker socket
func ConnectToSocket() (net.Conn, error) {
	c, err := net.Dial(socketNetwork, socketAddress)
	if err != nil {
		return nil, err
	}

	return c, nil
}

// VerifySocketConnection verifies whether the application can connect to the docker socket
func VerifySocketConnection() error {
	if _, err := os.Stat("/var/run/docker.sock"); errors.Is(err, os.ErrNotExist) {
		return err
	}

	c, err := ConnectToSocket()
	if err != nil {
		return err
	}

	err = c.Close()
	if err != nil {
		return err
	}

	return nil
}
