//go:build !darwin && !linux

package peerproc

func newPlatform() (Resolver, error) { return nil, ErrUnsupported }

func environ(int32) ([]string, error) { return nil, ErrUnsupported }

func processIdentity(int32) (Identity, error) { return Identity{}, ErrUnsupported }
