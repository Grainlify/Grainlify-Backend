package soroban

import (
	"testing"

	"github.com/stellar/go/keypair"
	"github.com/stellar/go/xdr"
)

func TestEncodeScValString(t *testing.T) {
	val, err := EncodeScValString("test")
	if err != nil {
		t.Fatalf("EncodeScValString failed: %v", err)
	}
	if val.Type != xdr.ScValTypeScvString {
		t.Errorf("expected ScvString, got %v", val.Type)
	}
	if val.Str == nil || *val.Str != "test" {
		t.Errorf("expected 'test', got %v", val.Str)
	}
}

func TestEncodeScValInt64(t *testing.T) {
	val, err := EncodeScValInt64(12345)
	if err != nil {
		t.Fatalf("EncodeScValInt64 failed: %v", err)
	}
	if val.Type != xdr.ScValTypeScvI64 {
		t.Errorf("expected ScvI64, got %v", val.Type)
	}
	if val.I64 == nil || *val.I64 != 12345 {
		t.Errorf("expected 12345, got %v", val.I64)
	}
}

func TestEncodeScValUint64(t *testing.T) {
	val, err := EncodeScValUint64(67890)
	if err != nil {
		t.Fatalf("EncodeScValUint64 failed: %v", err)
	}
	if val.Type != xdr.ScValTypeScvU64 {
		t.Errorf("expected ScvU64, got %v", val.Type)
	}
	if val.U64 == nil || *val.U64 != 67890 {
		t.Errorf("expected 67890, got %v", val.U64)
	}
}

func TestEncodeScValVec(t *testing.T) {
	vals := []xdr.ScVal{
		{Type: xdr.ScValTypeScvI64, I64: func() *xdr.Int64 { v := xdr.Int64(1); return &v }()},
		{Type: xdr.ScValTypeScvI64, I64: func() *xdr.Int64 { v := xdr.Int64(2); return &v }()},
	}
	
	vecVal, err := EncodeScValVec(vals)
	if err != nil {
		t.Fatalf("EncodeScValVec failed: %v", err)
	}
	if vecVal.Type != xdr.ScValTypeScvVec {
		t.Errorf("expected ScvVec, got %v", vecVal.Type)
	}
	if vecVal.Vec == nil {
		t.Fatal("expected non-nil Vec")
	}
	if len(**vecVal.Vec) != 2 {
		t.Errorf("expected vec length 2, got %d", len(**vecVal.Vec))
	}
}

func TestEncodeScSymbol(t *testing.T) {
	symbol, err := EncodeScSymbol("test_function")
	if err != nil {
		t.Fatalf("EncodeScSymbol failed: %v", err)
	}
	if symbol != "test_function" {
		t.Errorf("expected 'test_function', got %v", symbol)
	}
}

func TestEncodeContractAddress(t *testing.T) {
	// Test with hex string (64 chars = 32 bytes)
	hexID := "0000000000000000000000000000000000000000000000000000000000000000"
	addr, err := EncodeContractAddress(hexID)
	if err != nil {
		t.Fatalf("EncodeContractAddress failed with hex: %v", err)
	}
	if addr.Type != xdr.ScAddressTypeScAddressTypeContract {
		t.Errorf("expected Contract type, got %v", addr.Type)
	}
	if addr.ContractId == nil {
		t.Fatal("expected non-nil ContractId")
	}
}

func TestDefaultRetryConfig(t *testing.T) {
	config := DefaultRetryConfig()
	if config.MaxRetries != 3 {
		t.Errorf("expected MaxRetries 3, got %d", config.MaxRetries)
	}
	if config.InitialDelay.Seconds() != 1 {
		t.Errorf("expected InitialDelay 1s, got %v", config.InitialDelay)
	}
	if config.MaxDelay.Seconds() != 30 {
		t.Errorf("expected MaxDelay 30s, got %v", config.MaxDelay)
	}
	if config.BackoffMultiplier != 2.0 {
		t.Errorf("expected BackoffMultiplier 2.0, got %f", config.BackoffMultiplier)
	}
}

func TestDecodeScValInt64(t *testing.T) {
	val, _ := EncodeScValInt64(999)
	i, err := DecodeScValInt64(val)
	if err != nil {
		t.Fatalf("DecodeScValInt64 failed: %v", err)
	}
	if i != 999 {
		t.Errorf("expected 999, got %d", i)
	}

	valInvalid, _ := EncodeScValString("not an int")
	_, err = DecodeScValInt64(valInvalid)
	if err == nil {
		t.Error("expected error for invalid type, got nil")
	}
}

func TestDecodeScValString(t *testing.T) {
	valStr, _ := EncodeScValString("hello")
	s, err := DecodeScValString(valStr)
	if err != nil {
		t.Fatalf("DecodeScValString failed for string: %v", err)
	}
	if s != "hello" {
		t.Errorf("expected 'hello', got '%s'", s)
	}

	valInvalid, _ := EncodeScValInt64(42)
	_, err = DecodeScValString(valInvalid)
	if err == nil {
		t.Error("expected error for invalid type, got nil")
	}
}

func TestDecodeScValSymbol(t *testing.T) {
	valSym := xdr.ScVal{
		Type: xdr.ScValTypeScvSymbol,
		Sym:  func() *xdr.ScSymbol { s := xdr.ScSymbol("sym"); return &s }(),
	}
	s, err := DecodeScValSymbol(valSym)
	if err != nil {
		t.Fatalf("DecodeScValSymbol failed: %v", err)
	}
	if s != "sym" {
		t.Errorf("expected 'sym', got '%s'", s)
	}

	valInvalid, _ := EncodeScValInt64(42)
	_, err = DecodeScValSymbol(valInvalid)
	if err == nil {
		t.Error("expected error for invalid type, got nil")
	}
}

func TestDecodeScValAddress(t *testing.T) {
	kp, err := keypair.Random()
	if err != nil {
		t.Fatalf("failed to generate random key: %v", err)
	}
	addrStr := kp.Address()
	val, err := EncodeScValAddress(addrStr)
	if err != nil {
		t.Fatalf("EncodeScValAddress failed: %v", err)
	}

	s, err := DecodeScValAddress(val)
	if err != nil {
		t.Fatalf("DecodeScValAddress failed: %v", err)
	}
	if s != addrStr {
		t.Errorf("expected address '%s', got '%s'", addrStr, s)
	}

	valInvalid, _ := EncodeScValInt64(42)
	_, err = DecodeScValAddress(valInvalid)
	if err == nil {
		t.Error("expected error for invalid type, got nil")
	}
}

func TestDecodeScValAddress_EdgeCases(t *testing.T) {
	// 1. Address == nil
	valNilAddress := xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: nil}
	_, err := DecodeScValAddress(valNilAddress)
	if err == nil {
		t.Error("expected error for nil Address pointer, got nil")
	}

	// 2. AccountId == nil
	addrAccountNil := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: nil}
	valAccountNil := xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addrAccountNil}
	_, err = DecodeScValAddress(valAccountNil)
	if err == nil {
		t.Error("expected error for nil AccountId, got nil")
	}

	// 3. ContractId == nil
	addrContractNil := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: nil}
	valContractNil := xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addrContractNil}
	_, err = DecodeScValAddress(valContractNil)
	if err == nil {
		t.Error("expected error for nil ContractId, got nil")
	}

	// 4. Unknown address type
	addrUnknown := xdr.ScAddress{Type: xdr.ScAddressType(99)}
	valUnknown := xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addrUnknown}
	_, err = DecodeScValAddress(valUnknown)
	if err == nil {
		t.Error("expected error for unknown Address type, got nil")
	}
}

func TestDecodeScVal_NilPointers(t *testing.T) {
	// Int64 nil
	valInt := xdr.ScVal{Type: xdr.ScValTypeScvI64, I64: nil}
	_, err := DecodeScValInt64(valInt)
	if err == nil {
		t.Error("expected error for nil I64 pointer, got nil")
	}

	// String nil
	valStr := xdr.ScVal{Type: xdr.ScValTypeScvString, Str: nil}
	_, err = DecodeScValString(valStr)
	if err == nil {
		t.Error("expected error for nil Str pointer, got nil")
	}

	// Symbol nil
	valSym := xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: nil}
	_, err = DecodeScValSymbol(valSym)
	if err == nil {
		t.Error("expected error for nil Sym pointer, got nil")
	}
}

func TestDecodeScValStruct(t *testing.T) {
	// Success
	symKey := xdr.ScSymbol("key")
	strVal := xdr.ScString("value")
	scMap := xdr.ScMap{
		{
			Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &symKey},
			Val: xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &strVal},
		},
	}
	mapPtr := &scMap
	valMap := xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mapPtr}
	m, err := DecodeScValStruct(valMap)
	if err != nil {
		t.Fatalf("DecodeScValStruct failed: %v", err)
	}
	if len(m) != 1 {
		t.Errorf("expected map len 1, got %d", len(m))
	}

	// 1. Wrong type
	valInvalidType := xdr.ScVal{Type: xdr.ScValTypeScvI64}
	_, err = DecodeScValStruct(valInvalidType)
	if err == nil {
		t.Error("expected error for wrong type, got nil")
	}

	// 2. Map pointer nil
	valMapNil := xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: nil}
	_, err = DecodeScValStruct(valMapNil)
	if err == nil {
		t.Error("expected error for nil Map pointer, got nil")
	}

	// 3. *Map pointer nil
	var mapPtrNil *xdr.ScMap = nil
	valMapNil2 := xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mapPtrNil}
	_, err = DecodeScValStruct(valMapNil2)
	if err == nil {
		t.Error("expected error for nil *Map pointer, got nil")
	}

	// 4. Non-symbol key
	scMapInvalidKey := xdr.ScMap{
		{
			Key: xdr.ScVal{Type: xdr.ScValTypeScvI64},
			Val: xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &strVal},
		},
	}
	mapPtrInvalidKey := &scMapInvalidKey
	valMapInvalidKey := xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mapPtrInvalidKey}
	_, err = DecodeScValStruct(valMapInvalidKey)
	if err == nil {
		t.Error("expected error for non-symbol key, got nil")
	}

	// 5. Symbol key with nil pointer
	scMapNilKeySym := xdr.ScMap{
		{
			Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: nil},
			Val: xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &strVal},
		},
	}
	mapPtrNilKeySym := &scMapNilKeySym
	valMapNilKeySym := xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mapPtrNilKeySym}
	_, err = DecodeScValStruct(valMapNilKeySym)
	if err == nil {
		t.Error("expected error for nil Key symbol, got nil")
	}
}


