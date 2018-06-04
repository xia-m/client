// Copyright 2015 Keybase, Inc. All rights reserved. Use of
// this source code is governed by the included BSD license.

package libkb

import (
	"errors"
	"fmt"
	"time"

	keybase1 "github.com/keybase/client/go/protocol/keybase1"
	context "golang.org/x/net/context"
)

type timedGenericKey struct {
	Contextified
	key   GenericKey
	which string
	atime time.Time
}

func newTimedGenericKey(g *GlobalContext, k GenericKey, w string) *timedGenericKey {
	return &timedGenericKey{
		Contextified: NewContextified(g),
		key:          k,
		atime:        g.Clock().Now(),
		which:        w,
	}
}

func (t *timedGenericKey) getKey() GenericKey {
	t.atime = t.G().Clock().Now()
	return t.key
}

func (t *timedGenericKey) clean() {
	now := t.G().Clock().Now()
	if t.key != nil && (now.Sub(t.atime) > PaperKeyMemoryTimeout) {
		t.G().Log.Debug("Cleaned out key %q at %s", t.which, now)
		t.key = nil
	}
}

type Account struct {
	Contextified
	secretSyncer *SecretSyncer
	localSession *Session
	loginSession *LoginSession
	streamCache  *PassphraseStreamCache
	skbKeyring   *SKBKeyringFile

	paperSigKey *timedGenericKey // cached, unlocked paper signing key
	paperEncKey *timedGenericKey // cached, unlocked paper encryption key

	secretPromptCanceledAt time.Time // when the secret prompt was last canceled

	testPostCleanHook func() // for testing, call this hook after cleaning
}

// Account implements a LoginContext
var _ LoginContext = (*Account)(nil)

func NewAccount(g *GlobalContext) *Account {
	return &Account{
		localSession: newSession(g),
		secretSyncer: NewSecretSyncer(g),
		Contextified: NewContextified(g),
	}
}

func (a *Account) LocalSession() *Session {
	return a.localSession
}

func (a *Account) GetUID() (ret keybase1.UID) {
	if a.localSession != nil {
		ret = a.localSession.GetUID()
	}
	return ret
}

func (a *Account) GetUsername() (ret NormalizedUsername) {
	if a.localSession == nil {
		return ret
	}
	if a.localSession.username == nil {
		return ret
	}
	ret = *a.localSession.username
	return ret
}

func (a *Account) GetDeviceID() (ret keybase1.DeviceID) {
	if a.localSession != nil {
		ret = a.localSession.GetDeviceID()
	}
	return ret
}

func (a *Account) UnloadLocalSession() {
	a.localSession = newSession(a.G())
}

// LoggedIn returns true if the user is logged in.  It does not
// try to load the session.
func (a *Account) LoggedIn() bool {
	return a.G().ActiveDevice.Valid() || a.LocalSession().IsLoggedIn()
}

// LoggedInLoad will load and check the session with the api server if necessary.
func (a *Account) LoggedInLoad() (bool, error) {
	return a.LoggedIn(), nil
}

// LoggedInProvisioned will check if the user is logged in and provisioned on this
// device. It will do so by bootstrapping the ActiveDevice.
func (a *Account) LoggedInProvisioned(ctx context.Context) (bool, error) {
	_, err := BootstrapActiveDeviceFromConfig(NewMetaContext(ctx, a.G()).WithLoginContext(a), true)
	if err == nil {
		return true, nil
	}
	if _, isLRE := err.(LoginRequiredError); isLRE {
		return false, nil
	}
	return false, err
}

func (a *Account) LoadLoginSession(emailOrUsername string) error {
	if a.LoginSession().ExistsFor(emailOrUsername) && a.LoginSession().NotExpired() {
		return nil
	}

	ls := NewLoginSession(a.G(), emailOrUsername)
	if err := ls.Load(NewMetaContextBackground(a.G())); err != nil {
		return err
	}
	a.setLoginSession(ls)
	return nil
}

func (a *Account) CreateLoginSessionWithSalt(emailOrUsername string, salt []byte) error {
	if a.loginSessionExists() {
		return fmt.Errorf("CreateLoginSessionWithSalt called, but Account already has LoginSession")
	}

	ls := NewLoginSessionWithSalt(a.G(), emailOrUsername, salt)
	a.setLoginSession(ls)
	return nil
}

func (a *Account) setLoginSession(ls *LoginSession) {
	if a.loginSession != nil {
		// this usually happens in tests that don't call G.Logout() to logout.
		// But it probably signifies an error.
		a.G().Log.Debug("Account: overwriting loginSession")
	}
	a.loginSession = ls
}

func (a *Account) loginSessionExists() bool {
	return a.LoginSession() != nil
}

func (a *Account) LoginSession() *LoginSession {
	return a.loginSession
}

func (a *Account) Logout() error {
	a.G().Log.Debug("+ Account.Logout()")
	a.ClearStreamCache()

	err := a.localSession.Logout()
	if err != nil {
		a.G().Log.Debug("error in localSession.Logout(): %s", err)
		a.G().Log.Debug("(continuing with the rest of the logout process, but this error will be returned)")
	}

	a.UnloadLocalSession()
	a.loginSession = nil
	a.ClearKeyring()

	a.secretSyncer.Clear()
	a.secretSyncer = NewSecretSyncer(a.G())

	a.ClearCachedSecretKeys()

	a.G().Log.Debug("- Account.Logout() - all clears complete, localSession.Logout() -> %s", ErrToOk(err))

	return err
}

func (a *Account) CreateStreamCache(tsec Triplesec, pps *PassphraseStream) {
	if a.streamCache != nil {
		a.G().Log.Warning("Account.CreateStreamCache overwriting existing StreamCache")
	}
	a.streamCache = NewPassphraseStreamCache(tsec, pps)
}

func (a *Account) SetStreamCache(c *PassphraseStreamCache) {
	if a.streamCache != nil {
		a.G().Log.Warning("Account.CreateStreamCache overwriting existing StreamCache")
	}
	a.streamCache = c
}

// SetStreamGeneration sets the passphrase generation on the cached stream
// if it exists, and otherwise will wind up warning of a problem.
func (a *Account) SetStreamGeneration(gen PassphraseGeneration, nilPPStreamOK bool) {
	found := a.PassphraseStreamCache().MutatePassphraseStream(func(ps *PassphraseStream) {
		ps.SetGeneration(gen)
	})
	if !found && !nilPPStreamOK {
		a.G().Log.Warning("Passphrase stream was nil; unexpected")
	}
}

// GetStreamGeneration() gets the generation of the currently cached
// passphrase stream
func (a *Account) GetStreamGeneration() (ret PassphraseGeneration) {
	if ps := a.PassphraseStream(); ps != nil {
		ret = ps.Generation()
	}
	return
}

func (a *Account) CreateStreamCacheViaStretch(passphrase string) error {
	if a.streamCache.Valid() {
		return nil
	}

	salt, err := a.loginSession.Salt()
	if err != nil {
		return err
	}

	tsec, pps, err := StretchPassphrase(a.G(), passphrase, salt)
	if err != nil {
		return err
	}

	a.streamCache = NewPassphraseStreamCache(tsec, pps)

	return nil
}

func (a *Account) PassphraseStreamCache() *PassphraseStreamCache {
	return a.streamCache
}

// PassphraseStream returns a copy of the currently cached passphrase stream,
// or nil if none is there.
func (a *Account) PassphraseStream() *PassphraseStream {
	return a.PassphraseStreamCache().PassphraseStream()
}

func (a *Account) ClearStreamCache() {
	a.streamCache.Clear()
	a.streamCache = nil
}

// ClearLoginSession clears out any cached login sessions with the account
// object
func (a *Account) ClearLoginSession() {
	if a.loginSession != nil {
		// calling this is pointless since setting to nil next:
		a.loginSession.Clear()
		a.loginSession = nil
	}
}

func (a *Account) SecretSyncer() *SecretSyncer {
	return a.secretSyncer
}

func (a *Account) RunSecretSyncer(m MetaContext, uid keybase1.UID) error {
	return RunSyncer(m, a.SecretSyncer(), uid, a.LoggedIn(), a.localSession)
}

func (a *Account) Keyring(m MetaContext) (*SKBKeyringFile, error) {
	if a.localSession == nil {
		a.G().Log.Warning("local session is nil")
	}
	a.LocalSession().loadAndCheck()
	if a.localSession == nil {
		a.G().Log.Warning("local session after load is nil")
	}
	unp := a.localSession.GetUsername()
	var un NormalizedUsername
	if unp != nil {
		un = *unp
	}
	if un.IsNil() {
		un = a.G().Env.GetUsername()
	}

	// not sure how this could happen, but just in case:
	if un.IsNil() {
		return nil, NewNoUsernameError()
	}

	if a.skbKeyring != nil && a.skbKeyring.IsForUsername(un) {
		a.G().Log.Debug("Account: found loaded keyring for %s", un)
		return a.skbKeyring, nil
	}

	a.skbKeyring = nil

	a.G().Log.Debug("Account: loading keyring for %s", un)
	kr, err := LoadSKBKeyring(un, a.G())
	if err != nil {
		return nil, err
	}
	a.skbKeyring = kr
	return a.skbKeyring, nil
}

func (a *Account) LockedLocalSecretKey(ska SecretKeyArg) (*SKB, error) {
	return nil, errors.New("deprecated")
}

func (a *Account) Shutdown() error {
	return nil
}

func (a *Account) EnsureUsername(username NormalizedUsername) {
	su := a.LocalSession().GetUsername()
	if su == nil {
		a.LocalSession().SetUsername(username)
		return
	}
	if *su != username {
		a.Logout()
		a.LocalSession().SetUsername(username)
	}

}

// SaveState saves the logins state to memory, and to the user
// config file.
func (a *Account) SaveState(sessionID, csrf string, username NormalizedUsername, uid keybase1.UID, deviceID keybase1.DeviceID) error {
	if err := a.saveUserConfig(username, uid, deviceID); err != nil {
		return err
	}
	return a.LocalSession().SetLoggedIn(sessionID, csrf, username, uid, deviceID)
}

func (a *Account) saveUserConfig(username NormalizedUsername, uid keybase1.UID, deviceID keybase1.DeviceID) error {
	cw := a.G().Env.GetConfigWriter()
	if cw == nil {
		return NoConfigWriterError{}
	}

	// XXX I don't understand the intent of clearing the login session here.
	// All tests pass with this removed, but I'm wary of making any changes.
	// The git history didn't help, and this is the only place this function
	// is used (where it matters).
	if err := a.LoginSession().Clear(); err != nil {
		return err
	}
	salt, err := a.LoginSession().Salt()
	if err != nil {
		return err
	}

	// Note that `true` here means that an existing user config entry will
	// be overwritten.
	return cw.SetUserConfig(NewUserConfig(uid, username, salt, deviceID), true /* overwrite */)
}

func (a *Account) Dump(m MetaContext, prefix string) {
	fmt.Printf("Account dump:\n")
	a.loginSession.Dump()
	a.streamCache.Dump()
}

func (a *Account) SetLoginSession(l *LoginSession) {
	a.setLoginSession(l)
}

func (a *Account) SetCachedSecretKey(ska SecretKeyArg, key GenericKey, device *Device) error {
	return errors.New("deprecated")
}

func (a *Account) ClearCachedSecretKeys() {
	a.G().Log.Debug("clearing cached secret keys")
	a.ClearPaperKeys()
	if err := a.G().ActiveDevice.clear(a); err != nil {
		a.G().Log.Warning("error clearing ActiveDevice: %s", err)
	}
}

func (a *Account) ClearPaperKeys() {
	a.G().Log.Debug("clearing cached paper keys")
	a.paperEncKey = nil
	a.paperSigKey = nil
}

func (a *Account) SetTestPostCleanHook(f func()) {
	a.testPostCleanHook = f
}

func (a *Account) clean() {
	if a.paperEncKey != nil {
		a.paperEncKey.clean()
	}
	if a.paperSigKey != nil {
		a.paperSigKey.clean()
	}
	if a.testPostCleanHook != nil {
		a.testPostCleanHook()
	}
}

func (a *Account) ClearKeyring() {
	a.skbKeyring = nil
}

func (a *Account) SkipSecretPrompt() bool {
	if a.secretPromptCanceledAt.IsZero() {
		return false
	}

	if a.G().Clock().Now().Sub(a.secretPromptCanceledAt) < SecretPromptCancelDuration {
		return true
	}

	a.secretPromptCanceledAt = time.Time{}
	return false
}

func (a *Account) SecretPromptCanceled() {
	a.secretPromptCanceledAt = a.G().Clock().Now()
}

func (a *Account) SetDeviceName(name string) error {
	return a.G().ActiveDevice.setDeviceName(a, a.G().Env.GetUID(), a.localSession.GetDeviceID(), name)
}

func (a *Account) SetUsernameUID(n NormalizedUsername, u keybase1.UID) error {
	return errors.New("cannot call SetUsernameUID on legacy Account object")
}

func (a *Account) Salt() []byte { return nil }
