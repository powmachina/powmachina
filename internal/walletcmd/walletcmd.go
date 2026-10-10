// Package walletcmd holds what powmachina-wallet-cli and powmachina-wallet-rpc share:
// network selection, connecting to a node and reading a passphrase.
package walletcmd

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"powmachina/internal/chain"
)

// Network picks the network from the flags.
func Network(testnet, regtest bool) (chain.NetParams, error) {
	switch {
	case testnet && regtest:
		return chain.NetParams{}, errors.New("use either --testnet or --regtest, not both")
	case testnet:
		return chain.TestNet, nil
	case regtest:
		return chain.RegTest, nil
	}
	return chain.MainNet, nil
}

// DaemonAddress is the default node RPC address for a network.
func DaemonAddress(p chain.NetParams, flag string) string {
	if flag != "" {
		return flag
	}
	return fmt.Sprintf("127.0.0.1:%d", p.RPCPort)
}

// Stdin is the shared reader — the passphrase and the commands have to go through the same buffer.
var Stdin = bufio.NewReader(os.Stdin)

// ReadLine reads a line without the trailing \n. EOF on an empty line is an error.
func ReadLine(prompt string) (string, error) {
	fmt.Print(prompt)
	line, err := Stdin.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// ReadPassword reads a passphrase without echo (on a terminal), otherwise as a line (scripts).
func ReadPassword(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return ReadLine(prompt)
	}
	fmt.Print(prompt)
	b, err := term.ReadPassword(fd)
	fmt.Println()
	return string(b), err
}

// NewPassword asks for a new password twice.
func NewPassword() (string, error) {
	for {
		p1, err := ReadPassword("Enter a new password for the wallet: ")
		if err != nil {
			return "", err
		}
		if p1 == "" {
			fmt.Println("Password must not be empty.")
			continue
		}
		p2, err := ReadPassword("Confirm password: ")
		if err != nil {
			return "", err
		}
		if p1 != p2 {
			fmt.Println("Passwords do not match, try again.")
			continue
		}
		return p1, nil
	}
}
