package app

import "testing"

func TestDecryptAES256WrongKey(t *testing.T) {
	enc, err := EncryptAES256("yAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk=", "passphrase-of-the-old-installation")
	if err != nil {
		t.Fatal(err)
	}
	if plain, err := DecryptAES256(enc, "passphrase-of-the-old-installation"); err != nil ||
		plain != "yAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk=" {
		t.Fatalf("round trip failed: %q %v", plain, err)
	}
	// used to panic in pkcs7UnPadding (slice bounds out of range)
	if _, err := DecryptAES256(enc, "a-different-passphrase"); err == nil {
		t.Fatal("decrypting with the wrong passphrase must fail")
	}
}
