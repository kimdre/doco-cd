package compose

import (
	"errors"
	"fmt"
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

	defer func(c net.Conn) {
		err := c.Close()
		if err != nil {
			fmt.Printf("failed to close connection to docker socket: %v\n", err)
		}
	}(c)

	return c, nil
}

// VerifySocketConnection verifies whether the application can connect to the docker socket
func VerifySocketConnection() error {
	if _, err := os.Stat("/var/run/docker.sock"); errors.Is(err, os.ErrNotExist) {
		return err
	}

	_, err := ConnectToSocket()
	if err != nil {
		return err
	}

	return nil
}
