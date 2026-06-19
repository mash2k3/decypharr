//go:build windows

package nntp

import "syscall"

func (c *Client) socketControl() func(network, address string, rc syscall.RawConn) error {
	return nil
}
