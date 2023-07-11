package membership

import (
	"fmt"
	"strings"

	"github.com/multiformats/go-multiaddr"

	"github.com/consensus-shipyard/go-ipc-types/gateway"
	"github.com/consensus-shipyard/go-ipc-types/sdk"
	"github.com/consensus-shipyard/go-ipc-types/validator"

	"github.com/filecoin-project/go-address"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/builtin"
	"github.com/filecoin-project/go-state-types/crypto"

	mirproto "github.com/filecoin-project/mir/pkg/pb/trantorpb/types"
	tt "github.com/filecoin-project/mir/pkg/trantor/types"
	t "github.com/filecoin-project/mir/pkg/types"

	"github.com/filecoin-project/lotus/build"
	"github.com/filecoin-project/lotus/chain/actors"
	"github.com/filecoin-project/lotus/chain/ipcagent/rpc"
	"github.com/filecoin-project/lotus/chain/types"
)

const (
	FakeSource    string = "fake"
	StringSource  string = "string"
	FileSource    string = "file"
	OnChainSource string = "onchain"
)

func IsSourceValid(source string) error {
	switch strings.ToLower(source) {
	case FileSource:
		return nil
	case OnChainSource:
		return nil
	default:
		return fmt.Errorf("membership source %s noot supported", source)
	}
}

type Info struct {
	MinValidators uint64
	ValidatorSet  *validator.Set
	GenesisEpoch  uint64
}

type Reader interface {
	GetMembershipInfo() (*Info, error)
}

var _ Reader = &FileMembership{}

type FileMembership struct {
	FileName string
}

func NewFileMembership(fileName string) FileMembership {
	return FileMembership{
		FileName: fileName,
	}
}

// GetMembershipInfo gets the membership config from a file.
func (f FileMembership) GetMembershipInfo() (*Info, error) {
	vs, err := validator.NewValidatorSetFromFile(f.FileName)
	if err != nil {
		return nil, err
	}

	return &Info{
		ValidatorSet: vs,
	}, nil
}

// ------

var _ Reader = new(StringMembership)

type StringMembership string

// GetMembershipInfo gets the membership config from the input string.
func (s StringMembership) GetMembershipInfo() (*Info, error) {
	vs, err := validator.NewValidatorSetFromString(string(s))
	if err != nil {
		return nil, err
	}

	return &Info{
		ValidatorSet: vs,
	}, nil
}

// -----
var _ Reader = new(EnvMembership)

type EnvMembership string

// GetMembershipInfo gets the membership config from the input environment variable.
func (e EnvMembership) GetMembershipInfo() (*Info, error) {
	vs, err := validator.NewValidatorSetFromEnv(string(e))
	if err != nil {
		return nil, err
	}

	return &Info{
		ValidatorSet: vs,
	}, nil
}

// -----
var _ Reader = &OnChainMembership{}

type OnChainMembership struct {
	client rpc.JSONRPCRequestSender
	Subnet sdk.SubnetID
}

func NewOnChainMembershipClient(client rpc.JSONRPCRequestSender, subnet sdk.SubnetID) *OnChainMembership {
	return &OnChainMembership{
		client: client,
		Subnet: subnet,
	}
}

type AgentResponse struct {
	ValidatorSet  validator.Set `json:"validator_set"`
	MinValidators uint64        `json:"min_validators"`
	GenesisEpoch  uint64        `json:"genesis_epoch"`
}

// GetMembershipInfo gets the membership config from the actor state.
func (c *OnChainMembership) GetMembershipInfo() (*Info, error) {
	req := struct {
		Subnet string `json:"subnet"`
	}{
		Subnet: c.Subnet.String(),
	}

	var resp AgentResponse
	err := c.client.SendRequest("ipc_queryValidatorSet", &req, &resp)
	if err != nil {
		return nil, err
	}
	return &Info{
		ValidatorSet:  &resp.ValidatorSet,
		MinValidators: resp.MinValidators,
		GenesisEpoch:  resp.GenesisEpoch,
	}, nil
}

// ----

// Membership validates that validators addresses are correct multi-addresses and
// returns all the corresponding IDs and map between these IDs and the multi-addresses.
func Membership(validators []*validator.Validator) ([]t.NodeID, *mirproto.Membership, error) {
	var nodeIDs []t.NodeID
	nodeAddrs := make(map[t.NodeID]*mirproto.NodeIdentity)

	for _, v := range validators {
		id := t.NodeID(v.ID())
		a, err := multiaddr.NewMultiaddr(v.NetAddr)
		if err != nil {
			return nil, nil, err
		}
		nodeIDs = append(nodeIDs, id)
		nodeAddrs[id] = &mirproto.NodeIdentity{
			Id:     id,
			Addr:   a.String(),
			Key:    nil,
			Weight: tt.VoteWeight(v.Weight.Uint64()),
		}
	}

	membership := mirproto.Membership{
		Nodes: nodeAddrs,
	}

	return nodeIDs, &membership, nil
}

// NewSetMembershipMsg creates a new message to update implicitly
// the membership in the gateway actor of the subnet
func NewSetMembershipMsg(gw address.Address, valSet *validator.Set) (*types.SignedMessage, error) {
	params, err := actors.SerializeParams(valSet)
	if err != nil {
		return nil, err
	}
	msg := types.Message{
		To:         gw,
		From:       builtin.SystemActorAddr,
		Value:      abi.NewTokenAmount(0),
		Method:     builtin.MustGenerateFRCMethodNum("SetMembership"),
		Params:     params,
		GasFeeCap:  types.NewInt(0),
		GasPremium: types.NewInt(0),
		GasLimit:   build.BlockGasLimit, // Make super sure this is never too little
		Nonce:      0,
	}
	return &types.SignedMessage{Message: msg, Signature: crypto.Signature{Type: crypto.SigTypeDelegated}}, nil
}

// NewInitGenesisEpochMsg creates a new config message to initialize
// implicitly the subnet and set the genesis epoch for it.
func NewInitGenesisEpochMsg(gw address.Address, genesisEpoch abi.ChainEpoch) (*types.SignedMessage, error) {
	params, err := actors.SerializeParams(&gateway.InitGenesisEpochParams{GenesisEpoch: genesisEpoch})
	if err != nil {
		return nil, err
	}
	msg := types.Message{
		To:         gw,
		From:       builtin.SystemActorAddr,
		Value:      abi.NewTokenAmount(0),
		Method:     builtin.MustGenerateFRCMethodNum("InitGenesisEpoch"),
		Params:     params,
		GasFeeCap:  types.NewInt(0),
		GasPremium: types.NewInt(0),
		GasLimit:   build.BlockGasLimit, // Make super sure this is never too little
		// the nonce must be different from other config messages for the case where
		// all config messages are included in the same block, if not the one with the
		// largest nonce will be discarded.
		Nonce: 1,
	}
	return &types.SignedMessage{Message: msg, Signature: crypto.Signature{Type: crypto.SigTypeDelegated}}, nil
}

// IsConfigMsg determines if the message is an on-chain configuration message.
func IsConfigMsg(gw address.Address, msg *types.Message) bool {
	return IsSetMembershipConfigMsg(gw, msg) || IsInitGenesisEpochConfigMsg(gw, msg)
}

// IsSetMembershipConfigMsg determines if the message sets membership.
func IsSetMembershipConfigMsg(gw address.Address, msg *types.Message) bool {
	return msg.To == gw &&
		msg.From == builtin.SystemActorAddr &&
		msg.Method == builtin.MustGenerateFRCMethodNum("SetMembership")
}

// IsInitGenesisEpochConfigMsg determines if the message initializes the genesis epoch.
func IsInitGenesisEpochConfigMsg(gw address.Address, msg *types.Message) bool {
	return msg.To == gw &&
		msg.From == builtin.SystemActorAddr &&
		msg.Method == builtin.MustGenerateFRCMethodNum("InitGenesisEpoch")
}
