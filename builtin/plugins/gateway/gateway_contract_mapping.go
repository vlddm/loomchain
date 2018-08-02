// +build evm

package gateway

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/gogo/protobuf/proto"
	loom "github.com/loomnetwork/go-loom"
	tgtypes "github.com/loomnetwork/go-loom/builtin/types/transfer_gateway"
	"github.com/loomnetwork/go-loom/common/evmcompat"
	contract "github.com/loomnetwork/go-loom/plugin/contractpb"
	ssha "github.com/miguelmota/go-solidity-sha3"
)

type (
	PendingContractMapping             = tgtypes.TransferGatewayPendingContractMapping
	ContractAddressMapping             = tgtypes.TransferGatewayContractAddressMapping
	UnverifiedContractCreator          = tgtypes.TransferGatewayUnverifiedContractCreator
	VerifiedContractCreator            = tgtypes.TransferGatewayVerifiedContractCreator
	AddContractMappingRequest          = tgtypes.TransferGatewayAddContractMappingRequest
	UnverifiedContractCreatorsRequest  = tgtypes.TransferGatewayUnverifiedContractCreatorsRequest
	UnverifiedContractCreatorsResponse = tgtypes.TransferGatewayUnverifiedContractCreatorsResponse
	VerifyContractCreatorsRequest      = tgtypes.TransferGatewayVerifyContractCreatorsRequest
)

// AddContractMapping adds a mapping between a DAppChain contract and a Mainnet contract.
func (gw *Gateway) AddContractMapping(ctx contract.Context, req *AddContractMappingRequest) error {
	if req.ForeignContract == nil || req.LocalContract == nil || req.ForeignContractCreatorSig == nil ||
		req.ForeignContractTxHash == nil {
		return ErrInvalidRequest
	}
	foreignAddr := loom.UnmarshalAddressPB(req.ForeignContract)
	localAddr := loom.UnmarshalAddressPB(req.LocalContract)
	if foreignAddr.ChainID == "" || localAddr.ChainID == "" {
		return ErrInvalidRequest
	}
	if foreignAddr.Compare(localAddr) == 0 {
		return ErrInvalidRequest
	}

	state, err := loadState(ctx)
	if err != nil {
		return err
	}

	hash := ssha.SoliditySHA3(
		ssha.Address(common.BytesToAddress(req.ForeignContract.Local)),
		ssha.Address(common.BytesToAddress(req.LocalContract.Local)),
	)

	signerAddr, err := evmcompat.RecoverAddressFromTypedSig(hash, req.ForeignContractCreatorSig)
	if err != nil {
		return err
	}

	err = ctx.Set(pendingContractMappingKey(state.NextContractMappingID),
		&PendingContractMapping{
			ID:              state.NextContractMappingID,
			ForeignContract: req.ForeignContract,
			LocalContract:   req.LocalContract,
			ForeignContractCreator: loom.Address{
				ChainID: foreignAddr.ChainID,
				Local:   loom.LocalAddress(signerAddr.Bytes()),
			}.MarshalPB(),
			ForeignContractTxHash: req.ForeignContractTxHash,
		},
	)

	state.NextContractMappingID++
	return ctx.Set(stateKey, state)
}

func (gw *Gateway) UnverifiedContractCreators(ctx contract.StaticContext,
	req *UnverifiedContractCreatorsRequest) (*UnverifiedContractCreatorsResponse, error) {
	var creators []*UnverifiedContractCreator
	for _, entry := range ctx.Range(pendingContractMappingKeyPrefix) {
		var mapping PendingContractMapping
		if err := proto.Unmarshal(entry.Value, &mapping); err != nil {
			return nil, err
		}
		creators = append(creators, &UnverifiedContractCreator{
			ContractMappingID: mapping.ID,
			ContractTxHash:    mapping.ForeignContractTxHash,
		})
	}
	return &UnverifiedContractCreatorsResponse{
		Creators: creators,
	}, nil
}

func (gw *Gateway) VerifyContractCreators(ctx contract.Context,
	req *VerifyContractCreatorsRequest) error {
	if len(req.Creators) == 0 {
		return ErrInvalidRequest
	}

	if ok, _ := ctx.HasPermission(verifyCreatorsPerm, []string{oracleRole}); !ok {
		return ErrNotAuthorized
	}

	for _, creatorInfo := range req.Creators {
		mappingKey := pendingContractMappingKey(creatorInfo.ContractMappingID)
		mapping := &PendingContractMapping{}
		if err := ctx.Get(mappingKey, mapping); err != nil {
			if err == contract.ErrNotFound {
				continue
			}
			return err
		}

		confirmContractMapping(ctx, mappingKey, mapping, creatorInfo)
	}

	return nil
}

func confirmContractMapping(ctx contract.Context, pendingMappingKey []byte, mapping *PendingContractMapping,
	cofirmation *VerifiedContractCreator) error {
	// Clear out the pending mapping regardless of whether it's successfully confirmed or not
	ctx.Delete(pendingMappingKey)

	if (mapping.ForeignContractCreator.ChainId != cofirmation.Creator.ChainId) ||
		(mapping.ForeignContractCreator.Local.Compare(cofirmation.Creator.Local) != 0) ||
		(mapping.ForeignContract.ChainId != cofirmation.Contract.ChainId) ||
		(mapping.ForeignContract.Local.Compare(cofirmation.Contract.Local) != 0) {
		ctx.Logger().Debug("[Transfer Gateway] failed to verify foreign contract creator",
			"expected-contract", mapping.ForeignContractCreator.Local,
			"expected-creator", mapping.ForeignContractCreator.Local,
			"actual-contract", cofirmation.Contract.Local,
			"actual-creator", cofirmation.Creator.Local,
		)
		return nil
	}

	foreignContractAddr := loom.UnmarshalAddressPB(mapping.ForeignContract)
	localContractAddr := loom.UnmarshalAddressPB(mapping.LocalContract)
	err := ctx.Set(contractAddrMappingKey(foreignContractAddr), &ContractAddressMapping{
		From: mapping.ForeignContract,
		To:   mapping.LocalContract,
	})
	if err != nil {
		return err
	}
	err = ctx.Set(contractAddrMappingKey(localContractAddr), &ContractAddressMapping{
		From: mapping.LocalContract,
		To:   mapping.ForeignContract,
	})
	if err != nil {
		return err
	}
	return nil
}

// Returns the address of the DAppChain contract that corresponds to the given Ethereum address
func resolveToLocalContractAddr(ctx contract.StaticContext, foreignContractAddr loom.Address) (loom.Address, error) {
	var mapping ContractAddressMapping
	if err := ctx.Get(contractAddrMappingKey(foreignContractAddr), &mapping); err != nil {
		return loom.Address{}, err
	}
	return loom.UnmarshalAddressPB(mapping.To), nil
}

// Returns the address of the Ethereum contract that corresponds to the given DAppChain address
func resolveToForeignContractAddr(ctx contract.StaticContext, localContractAddr loom.Address) (loom.Address, error) {
	var mapping ContractAddressMapping
	if err := ctx.Get(contractAddrMappingKey(localContractAddr), &mapping); err != nil {
		return loom.Address{}, err
	}
	return loom.UnmarshalAddressPB(mapping.To), nil
}
