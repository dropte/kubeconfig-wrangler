package profile

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"

	"github.com/zalando/go-keyring"
)

const (
	keychainService = "kubeconfig-wrangler"
	keychainUser    = "encryption-key"
	keySize         = 32 // AES-256
)

// Encryptor handles encryption and decryption of sensitive profile fields
type Encryptor struct {
	key []byte
}

// NewEncryptor creates a new encryptor with a key stored in the OS keychain
func NewEncryptor() (*Encryptor, error) {
	key, err := getOrCreateKey()
	if err != nil {
		return nil, err
	}
	return &Encryptor{key: key}, nil
}

// getOrCreateKey retrieves the encryption key from the OS keychain,
// or generates and stores a new one if it doesn't exist
func getOrCreateKey() ([]byte, error) {
	// Try to get existing key from keychain
	encodedKey, err := keyring.Get(keychainService, keychainUser)
	if err == nil {
		// Key exists, decode and return it
		key, err := base64.StdEncoding.DecodeString(encodedKey)
		if err != nil {
			return nil, fmt.Errorf("failed to decode key from keychain: %w", err)
		}
		if len(key) != keySize {
			// Key is invalid, generate a new one
			return generateAndStoreKey()
		}
		return key, nil
	}

	// Key doesn't exist or error accessing keychain, generate new key
	if err == keyring.ErrNotFound {
		return generateAndStoreKey()
	}

	return nil, fmt.Errorf("failed to access keychain: %w", err)
}

// generateAndStoreKey creates a new random encryption key and stores it in the keychain
func generateAndStoreKey() ([]byte, error) {
	// Generate a cryptographically secure random key
	key := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("failed to generate encryption key: %w", err)
	}

	// Store the key in the keychain
	encodedKey := base64.StdEncoding.EncodeToString(key)
	if err := keyring.Set(keychainService, keychainUser, encodedKey); err != nil {
		return nil, fmt.Errorf("failed to store key in keychain: %w", err)
	}

	return key, nil
}

// Encrypt encrypts a plaintext string and returns a base64-encoded ciphertext
func (e *Encryptor) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}

	block, err := aes.NewCipher(e.key)
	if err != nil {
		return "", fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("failed to create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("failed to generate nonce: %w", err)
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt decrypts a base64-encoded ciphertext and returns the plaintext
func (e *Encryptor) Decrypt(encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}

	ciphertext, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("failed to decode ciphertext: %w", err)
	}

	block, err := aes.NewCipher(e.key)
	if err != nil {
		return "", fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("failed to create GCM: %w", err)
	}

	if len(ciphertext) < gcm.NonceSize() {
		return "", fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt: %w", err)
	}

	return string(plaintext), nil
}

// EncryptProfile encrypts sensitive fields in a profile
func (e *Encryptor) EncryptProfile(p *Profile) (*Profile, error) {
	encrypted := *p

	var err error

	// Encrypt Rancher password
	if p.Password != "" {
		encrypted.Password, err = e.Encrypt(p.Password)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt password: %w", err)
		}
	}

	// Encrypt Rancher token
	if p.Token != "" {
		encrypted.Token, err = e.Encrypt(p.Token)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt token: %w", err)
		}
	}

	// Encrypt AWS secret key
	if p.SecretKey != "" {
		encrypted.SecretKey, err = e.Encrypt(p.SecretKey)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt secret key: %w", err)
		}
	}

	// Encrypt AWS session token
	if p.SessionToken != "" {
		encrypted.SessionToken, err = e.Encrypt(p.SessionToken)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt session token: %w", err)
		}
	}

	// Encrypt static kubeconfig
	if p.Kubeconfig != "" {
		encrypted.Kubeconfig, err = e.Encrypt(p.Kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt kubeconfig: %w", err)
		}
	}

	return &encrypted, nil
}

// DecryptProfile decrypts sensitive fields in a profile
func (e *Encryptor) DecryptProfile(p *Profile) (*Profile, error) {
	decrypted := *p

	var err error

	// Decrypt Rancher password
	if p.Password != "" {
		decrypted.Password, err = e.Decrypt(p.Password)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt password: %w", err)
		}
	}

	// Decrypt Rancher token
	if p.Token != "" {
		decrypted.Token, err = e.Decrypt(p.Token)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt token: %w", err)
		}
	}

	// Decrypt AWS secret key
	if p.SecretKey != "" {
		decrypted.SecretKey, err = e.Decrypt(p.SecretKey)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt secret key: %w", err)
		}
	}

	// Decrypt AWS session token
	if p.SessionToken != "" {
		decrypted.SessionToken, err = e.Decrypt(p.SessionToken)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt session token: %w", err)
		}
	}

	// Decrypt static kubeconfig
	if p.Kubeconfig != "" {
		decrypted.Kubeconfig, err = e.Decrypt(p.Kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt kubeconfig: %w", err)
		}
	}

	return &decrypted, nil
}
