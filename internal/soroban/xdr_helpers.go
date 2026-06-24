package soroban

import (
	"fmt"

	"github.com/stellar/go/keypair"
	"github.com/stellar/go/txnbuild"
	"github.com/stellar/go/xdr"
)

// EncodeScValString encodes a string as ScVal
func EncodeScValString(s string) (xdr.ScVal, error) {
	// Convert string to ScSymbol or ScString
	// For now, we'll use ScString
	scStr := xdr.ScString(s)
	return xdr.ScVal{
		Type: xdr.ScValTypeScvString,
		Str:  &scStr,
	}, nil
}

// EncodeScValInt64 encodes an int64 as ScVal
func EncodeScValInt64(i int64) (xdr.ScVal, error) {
	i64 := xdr.Int64(i)
	return xdr.ScVal{
		Type: xdr.ScValTypeScvI64,
		I64:  &i64,
	}, nil
}

// EncodeScValUint64 encodes a uint64 as ScVal
func EncodeScValUint64(u uint64) (xdr.ScVal, error) {
	u64 := xdr.Uint64(u)
	return xdr.ScVal{
		Type: xdr.ScValTypeScvU64,
		U64:  &u64,
	}, nil
}

// EncodeScValAddress encodes an address string as ScVal
func EncodeScValAddress(addrStr string) (xdr.ScVal, error) {
	// Try parsing as account address first
	kp, err := keypair.ParseAddress(addrStr)
	if err == nil {
		// It's an account address
		accountID := kp.Address()
		accountIdXdr, err := xdr.AddressToAccountId(accountID)
		if err != nil {
			return xdr.ScVal{}, fmt.Errorf("failed to convert account address: %w", err)
		}
		scAddr := xdr.ScAddress{
			Type:      xdr.ScAddressTypeScAddressTypeAccount,
			AccountId: &accountIdXdr,
		}
		return xdr.ScVal{
			Type:    xdr.ScValTypeScvAddress,
			Address: &scAddr,
		}, nil
	}

	// Try as contract address (hex or base64)
	contractAddr, err := EncodeContractAddress(addrStr)
	if err == nil {
		return xdr.ScVal{
			Type:    xdr.ScValTypeScvAddress,
			Address: &contractAddr,
		}, nil
	}

	return xdr.ScVal{}, fmt.Errorf("invalid address format: %s", addrStr)
}

// EncodeScValVec encodes a slice of ScVal as ScVal vector
func EncodeScValVec(vals []xdr.ScVal) (xdr.ScVal, error) {
	vec := xdr.ScVec(vals)
	vecPtr := &vec
	return xdr.ScVal{
		Type: xdr.ScValTypeScvVec,
		Vec:  &vecPtr,
	}, nil
}

// EncodeScSymbol encodes a symbol (function name) as ScSymbol
func EncodeScSymbol(s string) (xdr.ScSymbol, error) {
	// ScSymbol is just a string in XDR
	return xdr.ScSymbol(s), nil
}

// DecodeScValInt64 extracts an int64 from an ScvI64 ScVal.
// Returns an error if the type does not match or the pointer is nil.
func DecodeScValInt64(v xdr.ScVal) (int64, error) {
	if v.Type != xdr.ScValTypeScvI64 {
		return 0, fmt.Errorf("expected ScvI64, got %v", v.Type)
	}
	if v.I64 == nil {
		return 0, fmt.Errorf("ScvI64 value is nil")
	}
	return int64(*v.I64), nil
}

// DecodeScValAddress extracts the Stellar strkey address (G… account or C… contract)
// from an ScvAddress ScVal.
func DecodeScValAddress(v xdr.ScVal) (string, error) {
	if v.Type != xdr.ScValTypeScvAddress {
		return "", fmt.Errorf("expected ScvAddress, got %v", v.Type)
	}
	if v.Address == nil {
		return "", fmt.Errorf("ScvAddress value is nil")
	}
	switch v.Address.Type {
	case xdr.ScAddressTypeScAddressTypeAccount:
		if v.Address.AccountId == nil {
			return "", fmt.Errorf("account address is nil")
		}
		return v.Address.AccountId.GetAccountID().Address(), nil
	case xdr.ScAddressTypeScAddressTypeContract:
		if v.Address.ContractId == nil {
			return "", fmt.Errorf("contract address is nil")
		}
		// Encode as a 64-char lowercase hex string (the canonical internal form).
		return fmt.Sprintf("%x", (*v.Address.ContractId)[:]), nil
	default:
		return "", fmt.Errorf("unknown address type: %v", v.Address.Type)
	}
}

// DecodeScValString extracts a string from an ScvString ScVal.
func DecodeScValString(v xdr.ScVal) (string, error) {
	if v.Type != xdr.ScValTypeScvString {
		return "", fmt.Errorf("expected ScvString, got %v", v.Type)
	}
	if v.Str == nil {
		return "", fmt.Errorf("ScvString value is nil")
	}
	return string(*v.Str), nil
}

// DecodeScValSymbol extracts the symbol string from an ScvSymbol ScVal.
func DecodeScValSymbol(v xdr.ScVal) (string, error) {
	if v.Type != xdr.ScValTypeScvSymbol {
		return "", fmt.Errorf("expected ScvSymbol, got %v", v.Type)
	}
	if v.Sym == nil {
		return "", fmt.Errorf("ScvSymbol value is nil")
	}
	return string(*v.Sym), nil
}

// DecodeScValStruct extracts the fields map (key→ScVal) from an ScvMap ScVal.
// It validates that the ScVal is indeed a map and that every key is an ScvSymbol.
func DecodeScValStruct(v xdr.ScVal) (map[string]xdr.ScVal, error) {
	if v.Type != xdr.ScValTypeScvMap {
		return nil, fmt.Errorf("expected ScvMap, got %v", v.Type)
	}
	if v.Map == nil || *v.Map == nil {
		return nil, fmt.Errorf("ScvMap value is nil")
	}
	m := **v.Map
	out := make(map[string]xdr.ScVal, len(m))
	for _, entry := range m {
		if entry.Key.Type != xdr.ScValTypeScvSymbol {
			return nil, fmt.Errorf("map key type %v is not ScvSymbol", entry.Key.Type)
		}
		if entry.Key.Sym == nil {
			return nil, fmt.Errorf("map key symbol is nil")
		}
		out[string(*entry.Key.Sym)] = entry.Val
	}
	return out, nil
}

// BuildInvokeHostFunctionOp builds an InvokeHostFunction operation for contract calls
func BuildInvokeHostFunctionOp(contractAddress xdr.ScAddress, functionName string, args []xdr.ScVal) (txnbuild.Operation, error) {
	symbol, err := EncodeScSymbol(functionName)
	if err != nil {
		return nil, fmt.Errorf("failed to encode function name: %w", err)
	}

	// Build InvokeContractArgs
	invokeArgs := xdr.InvokeContractArgs{
		ContractAddress: contractAddress,
		FunctionName:    symbol,
		Args:            args,
	}

	// Build HostFunction
	hostFunction := xdr.HostFunction{
		Type:           xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
		InvokeContract: &invokeArgs,
	}

	return &txnbuild.InvokeHostFunction{
		HostFunction: hostFunction,
	}, nil
}
