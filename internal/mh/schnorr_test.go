package mh

import (
	"encoding/hex"
	"testing"
)

func TestSignAndVerifyRoundtrip(t *testing.T) {
	sk := [32]byte{}
	for i := range sk {
		sk[i] = 7
	}
	pk, err := SchnorrPubkey(&sk)
	if err != nil {
		t.Fatal(err)
	}
	msg := [32]byte{}
	for i := range msg {
		msg[i] = 42
	}
	sig, err := SchnorrSign(&msg, &sk)
	if err != nil {
		t.Fatal(err)
	}
	if !SchnorrVerify(&msg, pk, sig) {
		t.Fatal("our own signature failed verification")
	}
}

// A signature must not be valid for another message, another key or after one bit has changed.
func TestRejectsTampering(t *testing.T) {
	sk := [32]byte{}
	for i := range sk {
		sk[i] = 7
	}
	pk, _ := SchnorrPubkey(&sk)
	msg := [32]byte{}
	for i := range msg {
		msg[i] = 42
	}
	sig, _ := SchnorrSign(&msg, &sk)

	other := msg
	other[0] ^= 1
	if SchnorrVerify(&other, pk, sig) {
		t.Fatal("the signature is valid for another message")
	}

	sk2 := [32]byte{}
	for i := range sk2 {
		sk2[i] = 9
	}
	pk2, _ := SchnorrPubkey(&sk2)
	if SchnorrVerify(&msg, pk2, sig) {
		t.Fatal("the signature is valid for another key")
	}

	bad := *sig
	bad[10] ^= 1
	if SchnorrVerify(&msg, pk, &bad) {
		t.Fatal("the signature is valid after a bit changed")
	}
}

// Nonsensical input has to return false, not crash.
func TestGarbageIsRejectedWithoutPanic(t *testing.T) {
	var zero32 [32]byte
	var zero64 [64]byte
	if SchnorrVerify(&zero32, &zero32, &zero64) {
		t.Fatal("zero input passed")
	}
	var ff32 [32]byte
	var ff64 [64]byte
	for i := range ff32 {
		ff32[i] = 0xff
	}
	for i := range ff64 {
		ff64[i] = 0xff
	}
	one := [32]byte{31: 1}
	if SchnorrVerify(&one, &ff32, &ff64) {
		t.Fatal("an invalid key passed")
	}
	if _, err := SchnorrSign(&one, &zero32); err == nil {
		t.Fatal("a zero private key does not exist, no signature should have come about")
	}
}

func TestCompressedPubkeyHasValidPrefix(t *testing.T) {
	sk := [32]byte{}
	for i := range sk {
		sk[i] = 7
	}
	pk, err := CompressedPubkey(&sk)
	if err != nil {
		t.Fatal(err)
	}
	if pk[0] != 2 && pk[0] != 3 {
		t.Fatalf("the prefix has to be 02 or 03, it is %02x", pk[0])
	}
	var zero [32]byte
	if _, err := CompressedPubkey(&zero); err == nil {
		t.Fatal("a zero key has no public key")
	}
}

func TestScalarAddIsModularAndRejectsZero(t *testing.T) {
	var a, b, zero [32]byte
	for i := range a {
		a[i], b[i] = 1, 2
	}
	sum, err := ScalarAdd(&a, &b)
	if err != nil {
		t.Fatal(err)
	}
	if *sum == zero {
		t.Fatal("the sum should not be zero")
	}
	if got, _ := ScalarAdd(&a, &zero); *got != a {
		t.Fatal("a + 0 != a")
	}
	if _, err := ScalarAdd(&zero, &zero); err == nil {
		t.Fatal("a zero result has to be refused")
	}
	// Input above the curve's order is refused, not reduced — BIP32 skips the index on it.
	overN := mustHex32(t, "fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364142")
	if _, err := ScalarAdd(&overN, &zero); err == nil {
		t.Fatal("a scalar equal to the curve's order should have been refused")
	}
}

// The official vectors from BIP340 (bip-0340/test-vectors.csv). Signature verification is consensus —
// it has to match what the rest of the world does, including the edge cases (a point off the curve,
// s equal to the order, sG−eP at infinity).
//
// Vectors 15–18 are not here: their message has a length other than 32 B and our API signs a 32-byte
// hash exclusively. The signing vectors are not checked — BIP340 generates them with aux_rand, we sign
// deterministically through RFC 6979. Signing is not consensus, verification is.
func TestBIP340Vectors(t *testing.T) {
	cases := []struct {
		name          string
		valid         bool
		pub, msg, sig string
	}{
		{"0 — a valid signature", true,
			"F9308A019258C31049344F85F89D5229B531C845836F99B08601F113BCE036F9",
			"0000000000000000000000000000000000000000000000000000000000000000",
			"E907831F80848D1069A5371B402410364BDF1C5F8307B0084C55F1CE2DCA8215" +
				"25F66A4A85EA8B71E482A74F382D2CE5EBEEE8FDB2172F477DF4900D310536C0"},
		{"1 — a valid signature", true,
			"DFF1D77F2A671C5F36183726DB2341BE58FEAE1DA2DECED843240F7B502BA659",
			"243F6A8885A308D313198A2E03707344A4093822299F31D0082EFA98EC4E6C89",
			"6896BD60EEAE296DB48A229FF71DFE071BDE413E6D43F917DC8DCF8C78DE3341" +
				"8906D11AC976ABCCB20B091292BFF4EA897EFCB639EA871CFA95F6DE339E4B0A"},
		{"2 — a valid signature", true,
			"DD308AFEC5777E13121FA72B9CC1B7CC0139715309B086C960E18FD969774EB8",
			"7E2D58D8B3BCDF1ABADEC7829054F90DDA9805AAB56C77333024B9D0A508B75C",
			"5831AAEED7B44BB74E5EAB94BA9D4294C49BCF2A60728D8B4C200F50DD313C1B" +
				"AB745879A5AD954A72C45A91C3A51D3C7ADEA98D82F8481E0E1E03674A6F3FB7"},
		{"3 — test fails if msg is reduced modulo p or n", true,
			"25D1DFF95105F5253C4022F628A996AD3A0D95FBF21D468A1B33F8C160D8F517",
			"FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF",
			"7EB0509757E246F19449885651611CB965ECC1A187DD51B64FDA1EDC9637D5EC" +
				"97582B9CB13DB3933705B32BA982AF5AF25FD78881EBB32771FC5922EFC66EA3"},
		{"4 — a valid signature", true,
			"D69C3509BB99E412E68B0FE8544E72837DFA30746D8BE2AA65975F29D22DC7B9",
			"4DF3C3F68FCC83B27E9D42C90431A72499F17875C81A599B566C9889B9696703",
			"00000000000000000000003B78CE563F89A0ED9414F5AA28AD0D96D6795F9C63" +
				"76AFB1548AF603B3EB45C9F8207DEE1060CB71C04E80F593060B07D28308D7F4"},
		{"5 — public key not on the curve", false,
			"EEFDEA4CDB677750A420FEE807EACF21EB9898AE79B9768766E4FAA04A2D4A34",
			"243F6A8885A308D313198A2E03707344A4093822299F31D0082EFA98EC4E6C89",
			"6CFF5C3BA86C69EA4B7376F31A9BCB4F74C1976089B2D9963DA2E5543E177769" +
				"69E89B4C5564D00349106B8497785DD7D1D713A8AE82B32FA79D5F7FC407D39B"},
		{"6 — has_even_y(R) is false", false,
			"DFF1D77F2A671C5F36183726DB2341BE58FEAE1DA2DECED843240F7B502BA659",
			"243F6A8885A308D313198A2E03707344A4093822299F31D0082EFA98EC4E6C89",
			"FFF97BD5755EEEA420453A14355235D382F6472F8568A18B2F057A1460297556" +
				"3CC27944640AC607CD107AE10923D9EF7A73C643E166BE5EBEAFA34B1AC553E2"},
		{"7 — negated message", false,
			"DFF1D77F2A671C5F36183726DB2341BE58FEAE1DA2DECED843240F7B502BA659",
			"243F6A8885A308D313198A2E03707344A4093822299F31D0082EFA98EC4E6C89",
			"1FA62E331EDBC21C394792D2AB1100A7B432B013DF3F6FF4F99FCB33E0E1515F" +
				"28890B3EDB6E7189B630448B515CE4F8622A954CFE545735AAEA5134FCCDB2BD"},
		{"8 — negated s value", false,
			"DFF1D77F2A671C5F36183726DB2341BE58FEAE1DA2DECED843240F7B502BA659",
			"243F6A8885A308D313198A2E03707344A4093822299F31D0082EFA98EC4E6C89",
			"6CFF5C3BA86C69EA4B7376F31A9BCB4F74C1976089B2D9963DA2E5543E177769" +
				"961764B3AA9B2FFCB6EF947B6887A226E8D7C93E00C5ED0C1834FF0D0C2E6DA6"},
		{"9 — sG - eP is infinite. Test fails in single verification if has_even_y(inf) is defined as true and x(inf) as 0", false,
			"DFF1D77F2A671C5F36183726DB2341BE58FEAE1DA2DECED843240F7B502BA659",
			"243F6A8885A308D313198A2E03707344A4093822299F31D0082EFA98EC4E6C89",
			"0000000000000000000000000000000000000000000000000000000000000000" +
				"123DDA8328AF9C23A94C1FEECFD123BA4FB73476F0D594DCB65C6425BD186051"},
		{"10 — sG - eP is infinite. Test fails in single verification if has_even_y(inf) is defined as true and x(inf) as 1", false,
			"DFF1D77F2A671C5F36183726DB2341BE58FEAE1DA2DECED843240F7B502BA659",
			"243F6A8885A308D313198A2E03707344A4093822299F31D0082EFA98EC4E6C89",
			"0000000000000000000000000000000000000000000000000000000000000001" +
				"7615FBAF5AE28864013C099742DEADB4DBA87F11AC6754F93780D5A1837CF197"},
		{"11 — sig[0:32] is not an X coordinate on the curve", false,
			"DFF1D77F2A671C5F36183726DB2341BE58FEAE1DA2DECED843240F7B502BA659",
			"243F6A8885A308D313198A2E03707344A4093822299F31D0082EFA98EC4E6C89",
			"4A298DACAE57395A15D0795DDBFD1DCB564DA82B0F269BC70A74F8220429BA1D" +
				"69E89B4C5564D00349106B8497785DD7D1D713A8AE82B32FA79D5F7FC407D39B"},
		{"12 — sig[0:32] is equal to field size", false,
			"DFF1D77F2A671C5F36183726DB2341BE58FEAE1DA2DECED843240F7B502BA659",
			"243F6A8885A308D313198A2E03707344A4093822299F31D0082EFA98EC4E6C89",
			"FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEFFFFFC2F" +
				"69E89B4C5564D00349106B8497785DD7D1D713A8AE82B32FA79D5F7FC407D39B"},
		{"13 — sig[32:64] is equal to curve order", false,
			"DFF1D77F2A671C5F36183726DB2341BE58FEAE1DA2DECED843240F7B502BA659",
			"243F6A8885A308D313198A2E03707344A4093822299F31D0082EFA98EC4E6C89",
			"6CFF5C3BA86C69EA4B7376F31A9BCB4F74C1976089B2D9963DA2E5543E177769" +
				"FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141"},
		{"14 — public key is not a valid X coordinate because it exceeds the field size", false,
			"FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEFFFFFC30",
			"243F6A8885A308D313198A2E03707344A4093822299F31D0082EFA98EC4E6C89",
			"6CFF5C3BA86C69EA4B7376F31A9BCB4F74C1976089B2D9963DA2E5543E177769" +
				"69E89B4C5564D00349106B8497785DD7D1D713A8AE82B32FA79D5F7FC407D39B"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pub := mustHex32(t, c.pub)
			msg := mustHex32(t, c.msg)
			raw, err := hex.DecodeString(c.sig)
			if err != nil || len(raw) != 64 {
				t.Fatalf("a bad signature in the vector: %v", err)
			}
			var sig [64]byte
			copy(sig[:], raw)
			if got := SchnorrVerify(&msg, &pub, &sig); got != c.valid {
				t.Fatalf("verification returned %v, expected %v", got, c.valid)
			}
		})
	}
}

func mustHex32(t *testing.T, s string) [32]byte {
	t.Helper()
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 32 {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	var out [32]byte
	copy(out[:], raw)
	return out
}
