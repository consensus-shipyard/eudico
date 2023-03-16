package mirvalidator

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	logging "github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/multiformats/go-multiaddr"
	"github.com/urfave/cli/v2"
	"golang.org/x/xerrors"

	lcli "github.com/filecoin-project/lotus/cli"
)

var log = logging.Logger("mir-validator-cli")

func repoInitialized(ctx context.Context, cctx *cli.Context) error {
	_, ncloser, err := lcli.GetFullNodeAPIV1(cctx)
	if err != nil {
		return xerrors.Errorf("checking api to see if repo initialized: %w", err)
	}
	defer ncloser()
	return nil

}
func initCheck(path string) error {
	isCfg, err := isConfigured(path)
	if err != nil {
		if isCfg {
			return fmt.Errorf("validator configured and config corrupted: %v. Backup the config files you want to keep and run `./eudico mir validator init -f`", err)
		}
		return fmt.Errorf("validator not configured. Run `./eudico mir validator config init`")
	}
	return nil
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return true, nil
}

// returns an error if the validator is not configured.
func isConfigured(repo string) (bool, error) {
	// FIXME: If we make these constants configurable it will break this.
	// careful with this!
	hasCfg := false
	for _, s := range configFiles {
		p := filepath.Join(repo, s)
		ok, err := fileExists(p)
		if err != nil {
			return hasCfg, err
		}
		if !ok {
			return hasCfg, fmt.Errorf("missing %v config file", p)
		}
		// if it has at least one config file, it has been configured
		// but something is corrupted.
		hasCfg = true
	}
	return hasCfg, nil
}

// TODO: Consider encrypting the file and adding cmds to handle mir keys.
func lp2pID(dir string) (crypto.PrivKey, error) {
	// See if the key exists.
	path := filepath.Join(dir, PrivKeyPath)
	// if it doesn't exist create a new key
	exists, err := fileExists(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		pk, err := genLibp2pKey()
		if err != nil {
			return nil, fmt.Errorf("error generating libp2p key: %w", err)
		}
		file, err := os.Create(path)
		if err != nil {
			return nil, fmt.Errorf("error creating libp2p key: %w", err)
		}
		kbytes, err := crypto.MarshalPrivateKey(pk)
		if err != nil {
			return nil, fmt.Errorf("error marshalling libp2p key: %w", err)
		}
		_, err = file.Write(kbytes)
		if err != nil {
			return nil, fmt.Errorf("error writing libp2p key in file: %w", err)
		}
		return pk, nil
	}
	kbytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("error reading libp2p key from file: %w", err)
	}

	// if read and return the key.
	return crypto.UnmarshalPrivateKey(kbytes)
}

func genLibp2pKey() (crypto.PrivKey, error) {
	pk, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		return nil, err
	}
	return pk, nil
}

// TODO: Should we make multiaddrs configurable?
func newLibP2PHost(dir string, port int) (host.Host, error) {
	pk, err := lp2pID(dir)
	if err != nil {
		return nil, err
	}
	// See if mutiaddr exists.
	path := filepath.Join(dir, MaddrPath)
	// if it doesn't exist create a new key
	exists, err := fileExists(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		// use any free endpoints in the host.
		h, err := libp2p.New(
			libp2p.Identity(pk),
			libp2p.DefaultTransports,
			libp2p.ListenAddrStrings(
				fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", port),
				fmt.Sprintf("/ip6/::/tcp/%d", port),
				fmt.Sprintf("/ip4/0.0.0.0/udp/%d/quic", port),
				fmt.Sprintf("/ip6/::/udp/%d/quic", port),
			),
		)
		if err != nil {
			return nil, err
		}
		file, err := os.Create(path)
		if err != nil {
			return nil, fmt.Errorf("error creating libp2p multiaddr: %w", err)
		}
		b, err := marshalMultiAddrSlice(h.Addrs())
		if err != nil {
			return nil, err
		}
		_, err = file.Write(b)
		if err != nil {
			return nil, fmt.Errorf("error writing libp2p multiaddr in file: %w", err)
		}
		return h, nil
	}
	bMaddr, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("error reading multiaddr from file: %w", err)
	}

	addrs, err := unmarshalMultiAddrSlice(bMaddr)
	if err != nil {
		return nil, err
	}
	return libp2p.New(
		libp2p.Identity(pk),
		libp2p.DefaultTransports,
		libp2p.ListenAddrs(addrs...),
	)
}

func getLibP2PHost(dir string) (host.Host, error) {
	pk, err := lp2pID(dir)
	if err != nil {
		return nil, err
	}
	// See if mutiaddr exists.
	path := filepath.Join(dir, MaddrPath)
	// if it doesn't exist create a new key
	exists, err := fileExists(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("libp2p file not found: %w", err)
	}

	bMaddr, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("error reading multiaddr from file: %w", err)
	}

	addrs, err := unmarshalMultiAddrSlice(bMaddr)
	if err != nil {
		return nil, err
	}
	return libp2p.New(
		libp2p.Identity(pk),
		libp2p.DefaultTransports,
		libp2p.ListenAddrs(addrs...),
	)
}

func marshalMultiAddrSlice(ma []multiaddr.Multiaddr) ([]byte, error) {
	out := []string{}
	for _, a := range ma {
		out = append(out, a.String())
	}
	return json.Marshal(&out)
}

func unmarshalMultiAddrSlice(b []byte) ([]multiaddr.Multiaddr, error) {
	out := []multiaddr.Multiaddr{}
	s := []string{}
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	for _, mstr := range s {
		a, err := multiaddr.NewMultiaddr(mstr)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}
