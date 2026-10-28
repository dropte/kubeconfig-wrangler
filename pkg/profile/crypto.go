package profile

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zalando/go-keyring"
)

const (
	keychainService = "kubeconfig-wrangler"
	keychainUser    = "encryption-key"
	keySize         = 32 // AES-256

	// keyFileName is the fallback key file inside ConfigDir, used when the
	// OS keyring is unavailable or KUBECONFIG_WRANGLER_KEYRING=file is set.
	keyFileName = "encryption.key"
	// keyringEnv selects the key source; "file" forces the key file.
	keyringEnv = "KUBECONFIG_WRANGLER_KEYRING"
)

// Encryptor handles encryption and decryption of sensitive profile fields
type Encryptor struct {
	key []byte
}

// NewEncryptor creates a new encryptor whose key comes from the OS keychain
// (or the fallback key file, see getOrCreateKey).
func NewEncryptor() (*Encryptor, error) {
	dir, err := ConfigDir()
	if err != nil {
		return nil, err
	}
	key, err := getOrCreateKeyLocked(dir, filepath.Join(dir, profilesFile))
	if err != nil {
		return nil, err
	}
	return &Encryptor{key: key}, nil
}

// NewEncryptorWithKey creates an encryptor from an in-memory key. It never
// touches the keychain and is intended for tests. The key must be 32 bytes.
func NewEncryptorWithKey(key []byte) *Encryptor {
	return &Encryptor{key: bytes.Clone(key)}
}

// keyringGet and keyringSet access the OS keychain. Tests replace them with
// an in-memory fake so they never touch the real keychain.
var (
	keyringGet = keyring.Get
	keyringSet = keyring.Set
)

// getOrCreateKeyLocked runs getOrCreateKey while holding the store's
// cross-process lock, so two processes starting at once cannot both see "no
// key" and create different keys.
func getOrCreateKeyLocked(keyDir, storePath string) ([]byte, error) {
	var key []byte
	err := withFileLock(lockPathFor(storePath), func() error {
		var err error
		key, err = getOrCreateKey(keyDir, storePath)
		return err
	})
	return key, err
}

// getOrCreateKey returns the encryption key. The OS keychain is used by
// default; if the keychain itself fails (anything other than "not found",
// e.g. Linux without a Secret Service) or KUBECONFIG_WRANGLER_KEYRING=file is
// set, a 0600 key file in keyDir is used instead. A new key is never
// generated while storePath holds encrypted secrets, because they would
// become permanently unreadable.
//
// When both the keychain and the key file hold a key (for example because
// the keychain was unavailable for a while and the fallback file was
// created), the two are reconciled by trying each against the encrypted
// fields in storePath; see reconcileKeys.
func getOrCreateKey(keyDir, storePath string) ([]byte, error) {
	keyFile := filepath.Join(keyDir, keyFileName)

	if os.Getenv(keyringEnv) == "file" {
		return getOrCreateFileKey(keyFile, storePath)
	}

	encodedKey, err := keyringGet(keychainService, keychainUser)
	switch {
	case err == nil:
		fileKey, fileErr := readFileKey(keyFile)
		key, decErr := base64.StdEncoding.DecodeString(encodedKey)
		if decErr == nil && len(key) == keySize {
			if fileErr != nil || fileKey == nil || bytes.Equal(fileKey, key) {
				return key, nil
			}
			return reconcileKeys(storePath, key, fileKey, keyFile)
		}
		// The keychain entry is unusable; a valid key file may still hold
		// the key the store was encrypted with.
		if fileErr == nil && fileKey != nil {
			return useFileKeyForInvalidKeychain(storePath, fileKey, keyFile)
		}
		if err := refuseIfEncrypted(storePath, "the keychain key is invalid"); err != nil {
			return nil, err
		}
		return generateAndStoreKey()
	case errors.Is(err, keyring.ErrNotFound):
		// A key file left from an earlier fallback run still holds the key.
		if _, statErr := os.Stat(keyFile); statErr == nil {
			return getOrCreateFileKey(keyFile, storePath)
		}
		if err := refuseIfEncrypted(storePath, "no key was found in the keychain"); err != nil {
			return nil, err
		}
		return generateAndStoreKey()
	default:
		// Transient or permanent keychain failure. An existing key file is
		// used as is; a new one is only created while the store holds no
		// encrypted data (getOrCreateFileKey refuses otherwise), and is
		// reconciled with the keychain key once the keychain is back.
		log.Printf("Warning: OS keychain unavailable (%v); using key file %s (set %s=file to always use it)", err, keyFile, keyringEnv)
		return getOrCreateFileKey(keyFile, storePath)
	}
}

// readFileKey returns the key held in keyFile, nil when the file does not
// exist, or an error when it cannot be read or does not hold a valid key.
func readFileKey(keyFile string) ([]byte, error) {
	data, err := os.ReadFile(keyFile)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(key) != keySize {
		return nil, fmt.Errorf("key file %s is invalid", keyFile)
	}
	return key, nil
}

// reconcileKeys picks between a valid keychain key and a different valid key
// file. Each key is tried against every encrypted profile in storePath:
//
//   - no encrypted data: the file key wins (it is what this machine falls
//     back to when the keychain fails) and is copied into the keychain so
//     both sources agree;
//   - the file key decrypts at least as many profiles as the keychain key:
//     the file key wins; it is copied into the keychain when the keychain
//     key decrypts nothing;
//   - otherwise the keychain key wins; a key file that decrypts nothing is
//     moved aside so a later keychain outage cannot encrypt new secrets with
//     a stale key;
//   - neither key decrypts anything: an error names both sources.
//
// The key file is never deleted.
func reconcileKeys(storePath string, keychainKey, fileKey []byte, keyFile string) ([]byte, error) {
	total, byKeychain, byFile, err := countDecryptable(storePath, keychainKey, fileKey)
	if err != nil {
		return nil, fmt.Errorf("cannot choose between the keychain key and %s: %w", keyFile, err)
	}

	switch {
	case total == 0:
		syncKeyToKeychain(fileKey, keyFile)
		return fileKey, nil
	case byKeychain == 0 && byFile == 0:
		return nil, fmt.Errorf("neither the OS keychain key nor key file %s decrypts the profiles in %s; restore the original key or move the file aside", keyFile, storePath)
	case byFile >= byKeychain:
		if byKeychain == 0 {
			syncKeyToKeychain(fileKey, keyFile)
		}
		if byFile < total {
			log.Printf("Warning: key file %s decrypts %d of %d profiles and the keychain key %d; using the key file", keyFile, byFile, total, byKeychain)
		} else {
			log.Printf("Warning: the OS keychain key differs from %s; using the key file, which decrypts the stored profiles", keyFile)
		}
		return fileKey, nil
	default:
		if byFile == 0 {
			aside := keyFile + "." + strconv.FormatInt(time.Now().Unix(), 10) + ".unused"
			if err := os.Rename(keyFile, aside); err != nil {
				log.Printf("Warning: key file %s does not match the stored profiles and could not be moved aside: %v", keyFile, err)
			} else {
				log.Printf("Warning: key file %s does not match the stored profiles; moved it to %s", keyFile, aside)
			}
		} else {
			log.Printf("Warning: keychain key decrypts %d of %d profiles and key file %s %d; using the keychain key", byKeychain, total, keyFile, byFile)
		}
		return keychainKey, nil
	}
}

// useFileKeyForInvalidKeychain handles an unusable keychain entry next to a
// valid key file: the file key is used (and copied into the keychain) unless
// the store holds encrypted data it cannot decrypt.
func useFileKeyForInvalidKeychain(storePath string, fileKey []byte, keyFile string) ([]byte, error) {
	total, byFile, _, err := countDecryptable(storePath, fileKey, nil)
	if err != nil {
		return nil, fmt.Errorf("the keychain key is invalid and %s cannot be checked: %w", keyFile, err)
	}
	if total > 0 && byFile == 0 {
		return nil, fmt.Errorf("the OS keychain key is invalid and key file %s does not decrypt the profiles in %s; restore the original key or move the file aside", keyFile, storePath)
	}
	log.Printf("Warning: the OS keychain key is invalid; using key file %s", keyFile)
	syncKeyToKeychain(fileKey, keyFile)
	return fileKey, nil
}

// syncKeyToKeychain copies key into the keychain, logging (not failing) when
// that is not possible.
func syncKeyToKeychain(key []byte, keyFile string) {
	if err := keyringSet(keychainService, keychainUser, base64.StdEncoding.EncodeToString(key)); err != nil {
		log.Printf("Warning: could not copy the key from %s into the OS keychain: %v", keyFile, err)
	}
}

// countDecryptable reports how many profiles in storePath hold encrypted
// fields, and how many of those decrypt with keyA and with keyB (a nil key
// counts zero).
func countDecryptable(storePath string, keyA, keyB []byte) (total, byA, byB int, err error) {
	data, err := os.ReadFile(storePath)
	if os.IsNotExist(err) {
		return 0, 0, 0, nil
	}
	if err != nil {
		return 0, 0, 0, err
	}
	var stored storeData
	if err := json.Unmarshal(data, &stored); err != nil {
		return 0, 0, 0, fmt.Errorf("failed to parse profiles file: %w", err)
	}
	decrypts := func(key []byte, p *Profile) bool {
		if key == nil {
			return false
		}
		_, err := NewEncryptorWithKey(key).DecryptProfile(p)
		return err == nil
	}
	for _, p := range stored.Profiles {
		if p == nil || !hasSecrets(p) {
			continue
		}
		total++
		if decrypts(keyA, p) {
			byA++
		}
		if decrypts(keyB, p) {
			byB++
		}
	}
	return total, byA, byB, nil
}

// getOrCreateFileKey reads the base64 key from keyFile, creating it with
// mode 0600 when absent (and the store holds no encrypted data).
func getOrCreateFileKey(keyFile, storePath string) ([]byte, error) {
	data, err := os.ReadFile(keyFile)
	if err == nil {
		key, decErr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
		if decErr == nil && len(key) == keySize {
			return key, nil
		}
		if err := refuseIfEncrypted(storePath, "key file "+keyFile+" is invalid"); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to read key file: %w", err)
	} else if err := refuseIfEncrypted(storePath, "key file "+keyFile+" does not exist"); err != nil {
		return nil, err
	}

	key, err := newRandomKey()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(keyFile), 0700); err != nil {
		return nil, fmt.Errorf("failed to create key directory: %w", err)
	}
	encoded := []byte(base64.StdEncoding.EncodeToString(key))
	if err := writeFileAtomic(keyFile, encoded); err != nil {
		return nil, fmt.Errorf("failed to write key file: %w", err)
	}
	return key, nil
}

// refuseIfEncrypted returns an error when storePath contains encrypted
// secrets, so a replacement key is not silently generated over them.
func refuseIfEncrypted(storePath, reason string) error {
	has, err := storeHasEncryptedData(storePath)
	if err != nil {
		return fmt.Errorf("refusing to generate a new encryption key (%s): cannot inspect %s: %w", reason, storePath, err)
	}
	if has {
		return fmt.Errorf("refusing to generate a new encryption key (%s): %s contains encrypted profiles that would become unreadable; restore the original key or move the file aside", reason, storePath)
	}
	return nil
}

// storeHasEncryptedData reports whether the profiles file at path exists and
// contains at least one non-empty encrypted field.
func storeHasEncryptedData(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var stored storeData
	if err := json.Unmarshal(data, &stored); err != nil {
		return false, fmt.Errorf("failed to parse profiles file: %w", err)
	}
	for _, p := range stored.Profiles {
		if p != nil && hasSecrets(p) {
			return true, nil
		}
	}
	return false, nil
}

func newRandomKey() ([]byte, error) {
	key := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("failed to generate encryption key: %w", err)
	}
	return key, nil
}

// generateAndStoreKey creates a new random encryption key and stores it in the keychain
func generateAndStoreKey() ([]byte, error) {
	key, err := newRandomKey()
	if err != nil {
		return nil, err
	}

	encodedKey := base64.StdEncoding.EncodeToString(key)
	if err := keyringSet(keychainService, keychainUser, encodedKey); err != nil {
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
