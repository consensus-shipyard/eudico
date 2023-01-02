package mir

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/ipfs/go-cid"
	u "github.com/ipfs/go-ipfs-util"
	"github.com/multiformats/go-multiaddr"
	"go.uber.org/zap/buffer"

	addr "github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	t "github.com/filecoin-project/mir/pkg/types"
)

type MembershipFromFile string
type MembershipFromEnv string
type MembershipFromStr string

type Validator struct {
	Addr addr.Address
	// FIXME: Consider using a multiaddr
	NetAddr string
}

func (v *Validator) ID() string {
	return v.Addr.String()
}

func (v *Validator) Bytes() ([]byte, error) {
	var b buffer.Buffer
	if err := v.MarshalCBOR(&b); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// ValidatorFromString parses a validator address from a string.
// OpaqueNetAddr can contain GRPC or libp2p addresses.
//
// Examples of validator strings:
//   - t1wpixt5mihkj75lfhrnaa6v56n27epvlgwparujy@/ip4/127.0.0.1/tcp/10000/p2p/12D3KooWJhKBXvytYgPCAaiRtiNLJNSFG5jreKDu2jiVpJetzvVJ
//   - t1wpixt5mihkj75lfhrnaa6v56n27epvlgwparujy@127.0.0.1:1000
//
// FIXME: Consider using json serde for this to support multiple multiaddr for validators.
func ValidatorFromString(input string) (Validator, error) {
	parts := strings.Split(input, "@")
	if len(parts) != 2 {
		return Validator{}, fmt.Errorf("failed to parse validators string")
	}
	ID := parts[0]
	opaqueNetAddr := parts[1]

	a, err := addr.NewFromString(ID)
	if err != nil {
		return Validator{}, err
	}
	ma, err := multiaddr.NewMultiaddr(opaqueNetAddr)
	if err != nil {
		return Validator{}, err
	}

	return Validator{
		Addr:    a,
		NetAddr: ma.String(),
	}, nil
}

func (v *Validator) String() string {
	return fmt.Sprintf("%s@%s", v.Addr.String(), v.NetAddr)
}

type ValidatorSet struct {
	Validators []Validator
}

func NewValidatorSet(vals []Validator) *ValidatorSet {
	return &ValidatorSet{Validators: vals}
}

func NewValidatorSetFromFile(path string) (*ValidatorSet, error) {
	return GetValidatorsFromFile(path)
}

func NewValidatorSetFromEnv(v string) (*ValidatorSet, error) {
	return GetValidatorsFromEnv(v)
}

func NewValidatorSetFromStr(s string) (*ValidatorSet, error) {
	return GetValidatorsFromStr(s)
}

func (set *ValidatorSet) Size() int {
	return len(set.Validators)
}

func (set *ValidatorSet) Equal(o *ValidatorSet) bool {
	if set == nil && o == nil {
		return true
	}
	if set == nil || o == nil {
		return true
	}
	if set.Size() != o.Size() {
		return false
	}
	for i, v := range set.Validators {
		if v != o.Validators[i] {
			return false
		}
	}
	return true
}

func (set *ValidatorSet) Hash() ([]byte, error) {
	var hs []byte
	for _, v := range set.Validators {
		b, err := v.Bytes()
		if err != nil {
			return nil, err
		}
		hs = append(hs, b...)
	}
	return cid.NewCidV0(u.Hash(hs)).Bytes(), nil
}

func (set *ValidatorSet) GetValidators() []Validator {
	return set.Validators
}

func (set *ValidatorSet) HasValidatorWithID(id string) bool {
	for _, v := range set.Validators {
		if v.ID() == id {
			return true
		}
	}
	return false
}

// BlockMiner returns a miner assigned deterministically using round-robin for a Filecoin epoch to assign a reward
// according to the rules of original Filecoin consensus.
func (set *ValidatorSet) BlockMiner(epoch abi.ChainEpoch) addr.Address {
	i := int(epoch) % set.Size()
	return set.Validators[i].Addr
}

func GetValidators(from interface{}) (*ValidatorSet, error) {
	switch v := from.(type) {
	case MembershipFromFile:
		return GetValidatorsFromFile(string(v))
	case MembershipFromEnv:
		return GetValidatorsFromEnv(string(v))
	case MembershipFromStr:
		return GetValidatorsFromStr(string(v))
	default:
		return nil, fmt.Errorf("unknown membership type")
	}
}

// GetValidatorsFromEnv gets the membership config from the environment variable.
func GetValidatorsFromEnv(env string) (*ValidatorSet, error) {
	var validators []Validator
	input := os.Getenv(env)
	if input == "" {
		return nil, fmt.Errorf("empty validator string")
	}

	for _, next := range splitAndTrimEmpty(input, ",", " ") {
		v, err := ValidatorFromString(next)
		if err != nil {
			return nil, err
		}

		validators = append(validators, v)
	}

	return NewValidatorSet(validators), nil
}

// GetValidatorsFromStr gets the membership config from the environment variable.
func GetValidatorsFromStr(input string) (*ValidatorSet, error) {
	var validators []Validator
	if input == "" {
		return nil, fmt.Errorf("empty validator string")
	}

	for _, next := range splitAndTrimEmpty(input, ",", " ") {
		v, err := ValidatorFromString(next)
		if err != nil {
			return nil, err
		}

		validators = append(validators, v)
	}

	return NewValidatorSet(validators), nil
}

func splitAndTrimEmpty(s, sep, cutset string) []string {
	if s == "" {
		return []string{}
	}

	spl := strings.Split(s, sep)
	nonEmptyStrings := make([]string, 0, len(spl))

	for i := 0; i < len(spl); i++ {
		element := strings.Trim(spl[i], cutset)
		if element != "" {
			nonEmptyStrings = append(nonEmptyStrings, element)
		}
	}

	return nonEmptyStrings
}

// GetValidatorsFromFile gets the membership config from a file.
func GetValidatorsFromFile(path string) (*ValidatorSet, error) {
	var validators []Validator

	_, err := os.Stat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no membership config found in path: %s", path)
	}
	readFile, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	defer func() {
		err := readFile.Close()
		if err != nil {
			log.Warnf("error closing membership config file: %w", err)
		}
	}()

	fileScanner := bufio.NewScanner(readFile)
	fileScanner.Split(bufio.ScanLines)

	for fileScanner.Scan() {
		v, err := ValidatorFromString(fileScanner.Text())
		if err != nil {
			return nil, err
		}
		validators = append(validators, v)
	}
	return NewValidatorSet(validators), nil
}

// ValidatorsToCfg creates validator config or appends to it
func ValidatorsToCfg(set *ValidatorSet, config string) error {
	f, err := os.OpenFile(config, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		panic(err)
	}

	defer func() {
		err := f.Close()
		if err != nil {
			log.Warnf("error closing membership config file: %w", err)
		}
	}()

	for _, v := range set.Validators {
		if _, err = f.WriteString(v.String() + "\n"); err != nil {
			panic(err)
		}
	}
	return nil
}

// validatorsMembership validates that validators addresses are correct multi-addresses and
// returns all the corresponding IDs and map between these IDs and the multi-addresses.
func validatorsMembership(validators []Validator) ([]t.NodeID, map[t.NodeID]t.NodeAddress, error) {
	var nodeIDs []t.NodeID
	nodeAddrs := make(map[t.NodeID]t.NodeAddress)

	for _, v := range validators {
		id := t.NodeID(v.ID())
		a, err := multiaddr.NewMultiaddr(v.NetAddr)
		if err != nil {
			return nil, nil, err
		}
		nodeIDs = append(nodeIDs, id)
		nodeAddrs[id] = a
	}

	return nodeIDs, nodeAddrs, nil
}
