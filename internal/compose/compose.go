package compose

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
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

func GetSocketGroupOwner() (string, error) {
	fi, err := os.Stat(socketAddress)
	if err != nil {
		return "", err
	}

	return strconv.Itoa(int(fi.Sys().(*syscall.Stat_t).Gid)), nil
}

// VerifySocketRead verifies whether the application can read from the docker socket
// by getting and printing a list of containers from the docker api
func VerifySocketRead(c net.Conn) error {
	requestData := []byte("GET http://localhost/v1.46/containers/json HTTP/1.1\r\nHost: localhost\r\n\r\n")
	maxRetries := 3

	var err error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		_, err = c.Write(requestData)
		if err != nil {
			var opErr *net.OpError
			if errors.As(err, &opErr) && opErr.Err.Error() == "write: broken pipe" {
				// Attempt to reconnect
				c, err = ConnectToSocket()
				if err != nil {
					continue // Retry connecting
				}
				// Successfully reconnected, try writing again
			}
		} else {
			// Write successful, proceed to read
			break
		}
	}

	if err != nil {
		return err // Return the last error encountered
	}

	responseBuffer := make([]byte, 65536) // Adjust buffer size as needed

	_, err = c.Read(responseBuffer)
	if err != nil {
		return err
	}

	_, err = os.Stdout.Write(responseBuffer)
	if err != nil {
		return err
	}

	return nil
}

// VerifySocketWrite verifies whether the application can write to the docker socket by sending a POST request
func VerifySocketWrite(c net.Conn) error {
	requestData := []byte("POST http://localhost/v1.46/containers/create HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: 0\r\n\r\n")

	_, err := c.Write(requestData)
	if err != nil {
		return err
	}

	responseBuffer := make([]byte, 65536) // Adjust buffer size as needed

	_, err = c.Read(responseBuffer)
	if err != nil {
		return err
	}

	_, err = os.Stdout.Write(responseBuffer)
	if err != nil {
		return err
	}

	return nil
}

// VerifySocketConnection verifies whether the application can connect to the docker socket
func VerifySocketConnection() error {
	if _, err := os.Stat("/var/run/docker.sock"); errors.Is(err, os.ErrNotExist) {
		return err
	}

	c, err := ConnectToSocket()

	// If ErrPermissionDenied is returned, return the required permissions
	if errors.Is(err, os.ErrPermission) {
		gid, err := GetSocketGroupOwner()
		if err != nil {
			return err
		}

		return fmt.Errorf("%v: current user needs group id %v", os.ErrPermission, gid)
	} else if err != nil {
		return err
	}

	//err = VerifySocketRead(c)
	//if err != nil {
	//	return err
	//}
	//
	//err = VerifySocketWrite(c)
	//if err != nil {
	//	return err
	//}

	err = c.Close()
	if err != nil {
		return err
	}

	return nil
}
