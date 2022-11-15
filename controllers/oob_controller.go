/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/sethvargo/go-password/password"
	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	oobv1alpha1 "github.com/onmetal/oob-operator/api/v1alpha1"
	"github.com/onmetal/oob-operator/bmc"
	"github.com/onmetal/oob-operator/log"
)

// OOBReconciler reconciles a OOB object
type OOBReconciler struct {
	client.Client
	Namespace            string
	CredentialsExpBuffer time.Duration
	ShutdownTimeout      time.Duration
	macPrefixes          prefixMap
	usernamePrefix       string
	usernameRegex        *regexp.Regexp
	temporaryPassword    string
	ntpServers           []string
}

//+kubebuilder:rbac:groups=onmetal.de,resources=oobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=onmetal.de,resources=oobs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=onmetal.de,resources=oobs/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete

type tag struct {
	Key   string `yaml:"key"`
	Value string `yaml:"value"`
}

type accessInfo struct {
	Protocol           string            `yaml:"protocol"`
	Tags               []tag             `yaml:"tags"`
	Port               int               `yaml:"port"`
	DefaultCredentials []bmc.Credentials `yaml:"defaultCredentials"`
	UUIDSource         string            `yaml:"uuidSource"`
}

type prefixMap map[string]accessInfo

func (m prefixMap) getAccessInfo(mac string) accessInfo {
	for i := len(mac); i > 0; i-- {
		if mac[i-1] == ':' {
			continue
		}
		prefix := mac[:i]
		l, ok := m[prefix]
		if ok {
			return l
		}
	}
	return accessInfo{}
}

// LoadMACPrefixes loads MAC address prefixes from a file.
func (r *OOBReconciler) LoadMACPrefixes(ctx context.Context, prefixesFile string) error {
	type macPrefixEntry struct {
		MACPrefix  string     `yaml:"macPrefix"`
		AccessInfo accessInfo `yaml:",inline"`
	}

	type macPrefixesConfig struct {
		TemporaryPassword string           `yaml:"temporaryPassword"`
		UsernamePrefix    string           `yaml:"usernamePrefix"`
		MACPrefixes       []macPrefixEntry `yaml:"macPrefixes"`
		NTPServers        []string         `yaml:"ntpServers"`
	}

	prefixesData, err := os.ReadFile(prefixesFile)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", prefixesFile, err)
	}

	var config macPrefixesConfig
	err = yaml.Unmarshal(prefixesData, &config)
	if err != nil {
		return fmt.Errorf("cannot unmarshal %s: %w", prefixesFile, err)
	}

	if config.TemporaryPassword == "" {
		return fmt.Errorf("a temporary password must be provided in the MAC prefixes configuration")
	}
	r.temporaryPassword = config.TemporaryPassword

	r.usernamePrefix = config.UsernamePrefix
	if r.usernamePrefix == "" {
		r.usernamePrefix = "metal-"
	}
	r.usernameRegex, err = regexp.Compile(r.usernamePrefix + `[a-z]{6}`)
	if err != nil {
		return fmt.Errorf("cannot compile username regex: %w", err)
	}

	if len(config.NTPServers) == 0 {
		return fmt.Errorf("a list of NTP servers must be provided")
	}
	r.ntpServers = config.NTPServers

	r.macPrefixes = make(map[string]accessInfo, len(config.MACPrefixes))
	for _, e := range config.MACPrefixes {
		r.macPrefixes[e.MACPrefix] = e.AccessInfo
	}

	log.Info(ctx, "Loaded MAC prefixes", "count", len(r.macPrefixes))
	return nil
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *OOBReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var oob oobv1alpha1.OOB
	err := r.Get(ctx, req.NamespacedName, &oob)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(fmt.Errorf("cannot get OOB: %w", err))
	}

	return r.reconcile(ctx, &oob)
}

func (r *OOBReconciler) reconcile(ctx context.Context, oob *oobv1alpha1.OOB) (ctrl.Result, error) {
	ctx = log.WithValues(ctx, "mac", oob.Spec.Mac, "ip", oob.Spec.IP, "uuid", oob.Spec.UUID)
	log.Debug(ctx, "Reconciling")

	// Clear None fields
	updated, err := r.clearNoneFields(ctx, oob)
	if err != nil {
		return ctrl.Result{}, err
	}
	// An update will trigger a new reconciliation
	if updated {
		log.Debug(ctx, "Reconciled successfully")
		return ctrl.Result{}, nil
	}

	// Ensure that the OOB has working persisted credentials
	var bmctrl bmc.BMC
	bmctrl, updated, err = r.ensureGoodCredentials(ctx, oob)
	if err != nil {
		return ctrl.Result{}, err
	}
	ctx = log.WithValues(ctx, "proto", bmctrl.Type())
	if updated {
		log.Debug(ctx, "Reconciled successfully")
		return ctrl.Result{}, nil
	}

	// Read OOB info
	log.Debug(ctx, "Retrieving OOB information")
	var info bmc.Info
	info, err = bmctrl.ReadInfo(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("cannot retrieve OOB information: %w", err)
	}

	// Ensure that the OOB has the correct name and UUID
	updated, err = r.ensureCorrectUUIDandName(ctx, oob, info.UUID, bmctrl.Credentials())
	if err != nil {
		return ctrl.Result{}, err
	}
	ctx = log.WithValues(ctx, "uuid", oob.Spec.UUID)
	if updated {
		log.Debug(ctx, "Reconciled successfully")
		return ctrl.Result{}, nil
	}

	// Set NTP servers
	err = r.setNTPServers(ctx, bmctrl)
	if err != nil {
		return ctrl.Result{}, err
	}

	specChanged := false
	requeueAfter := time.Hour * 24

	// Set all status fields
	statusChanged := r.setStatusFields(oob, &info, &requeueAfter)

	// Apply any changes to the locator LED
	err = r.applyLocatorLED(ctx, oob, bmctrl, &specChanged, &statusChanged)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Apply anu changes to the power state
	err = r.applyPower(ctx, oob, bmctrl, &specChanged, &statusChanged, &requeueAfter)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Apply any changes to the OOB spec
	if specChanged {
		oobSpec := &oobv1alpha1.OOB{
			TypeMeta: metav1.TypeMeta{
				APIVersion: oobv1alpha1.GroupVersion.String(),
				Kind:       "OOB",
			},
			ObjectMeta: metav1.ObjectMeta{
				Namespace: oob.Namespace,
				Name:      oob.Name,
			},
			Spec: oobv1alpha1.OOBSpec{
				LocatorLED: oob.Spec.LocatorLED,
				Power:      oob.Spec.Power,
				Reset:      oob.Spec.Reset,
			},
		}

		// Apply the OOB
		log.Info(ctx, "Applying OOB")
		err = r.Patch(ctx, oobSpec, client.Apply, client.FieldOwner("oob-operator.onmetal.de/oob/machine"), client.ForceOwnership)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("cannot apply OOB: %w", err)
		}
		oob = oobSpec
	}

	// Apply any changes to the OOB spec
	if statusChanged {
		oobStatus := &oobv1alpha1.OOB{
			TypeMeta: metav1.TypeMeta{
				APIVersion: oobv1alpha1.GroupVersion.String(),
				Kind:       "OOB",
			},
			ObjectMeta: metav1.ObjectMeta{
				Namespace: oob.Namespace,
				Name:      oob.Name,
			},
			Status: oob.Status,
		}

		// Apply the OOB
		log.Info(ctx, "Applying OOB status")
		err = r.Status().Patch(ctx, oobStatus, client.Apply, client.FieldOwner("oob-operator.onmetal.de/oob/machine"), client.ForceOwnership)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("cannot apply OOB: %w", err)
		}
		oob = oobStatus
	}

	log.Debug(ctx, "Reconciled successfully")
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *OOBReconciler) clearNoneFields(ctx context.Context, oob *oobv1alpha1.OOB) (bool, error) {
	// Replace all None fields with blanks in order to delete the fields.
	// This dance is necessary because one cannot delete a field if there happens to be another owner.
	hasNone := false
	if oob.Spec.LocatorLED == "None" {
		oob.Spec.LocatorLED = ""
		hasNone = true
	}
	if oob.Spec.Power == "None" {
		oob.Spec.Power = ""
		hasNone = true
	}
	if oob.Spec.Reset == "None" {
		oob.Spec.Reset = ""
		hasNone = true
	}
	if !hasNone {
		return false, nil
	}

	oob = &oobv1alpha1.OOB{
		TypeMeta: metav1.TypeMeta{
			APIVersion: oobv1alpha1.GroupVersion.String(),
			Kind:       "OOB",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: oob.Namespace,
			Name:      oob.Name,
		},
		Spec: oobv1alpha1.OOBSpec{
			LocatorLED: oob.Spec.LocatorLED,
			Power:      oob.Spec.Power,
			Reset:      oob.Spec.Reset,
		},
	}

	// Apply the OOB
	log.Info(ctx, "Applying OOB")
	err := r.Patch(ctx, oob, client.Apply, client.FieldOwner("oob-operator.onmetal.de/oob/machine"), client.ForceOwnership)
	if err != nil {
		return false, fmt.Errorf("cannot apply OOB: %w", err)
	}

	return true, nil
}

func (r *OOBReconciler) ensureGoodCredentials(ctx context.Context, oob *oobv1alpha1.OOB) (bmc.BMC, bool, error) {
	// Read the credentials secret if one exists
	creds, err := r.getCredentials(ctx, oob)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, false, fmt.Errorf("cannot get credentials: %w", err)
	}
	expireCreds := false

	// If the type is unknown, attempt to determine it
	if oob.Spec.Protocol == "" {
		log.Debug(ctx, "Determining OOB protocol")
		ai := r.macPrefixes.getAccessInfo(oob.Spec.Mac)
		if ai.Protocol == "" {
			return nil, false, fmt.Errorf("no known way of connecting to the OOB")
		}
		oob.Spec.Protocol = ai.Protocol
		oob.Spec.Tags = r.tagsToK8s(ai.Tags)
		oob.Spec.Port = ai.Port
		expireCreds = true
	}
	ctx = log.WithValues(ctx, "proto", oob.Spec.Protocol)

	// Initialize BMC
	var tags map[string]string
	tags, err = r.tagMapFromK8s(oob.Spec.Tags)
	if err != nil {
		return nil, false, fmt.Errorf("cannot parse OOB tags: %w", err)
	}
	var bmctrl bmc.BMC
	bmctrl, err = bmc.NewBMC(oob.Spec.Protocol, tags, oob.Spec.IP, oob.Spec.Port, creds)
	if err != nil {
		return nil, false, fmt.Errorf("cannot initialize BMC: %w", err)
	}

	// If credentials are unknown create new ones, otherwise try connecting
	if creds.Username == "" && creds.Password == "" {
		log.Info(ctx, "Ensuring initial credentials")
		ai := r.macPrefixes.getAccessInfo(oob.Spec.Mac)
		err = bmctrl.EnsureInitialCredentials(ctx, ai.DefaultCredentials, r.temporaryPassword)
		if err != nil {
			return nil, false, fmt.Errorf("cannot ensure initial credentials: %w", err)
		}
		expireCreds = true
	} else {
		err = bmctrl.Connect(ctx)
		if err != nil {
			return nil, false, fmt.Errorf("cannot connect to BMC: %w", err)
		}
	}

	// If the type had to be determined or the credentials created, expire the credentials to get fresh ones
	now := time.Now()
	if expireCreds {
		oob.Spec.PasswordExpiration = &metav1.Time{Time: now}
	}

	// If the credentials have expired (or are initial) create a new set of credentials
	if !oob.Spec.PasswordExpiration.IsZero() {
		timeToRenew := oob.Spec.PasswordExpiration.Add(-r.CredentialsExpBuffer)
		if timeToRenew.Before(now) {
			log.Info(ctx, "Creating new credentials", "expired", oob.Spec.PasswordExpiration)

			// Create new credentials
			var exp time.Time
			exp, err = r.createCredentials(ctx, bmctrl)
			if err != nil {
				return nil, false, fmt.Errorf("cannot create new credentials: %w", err)
			}
			oob.Spec.PasswordExpiration = &metav1.Time{Time: exp}
			ctx = log.WithValues(ctx, "expiration", oob.Spec.PasswordExpiration)

			// Persist the new credentials in case any upcoming operations fail
			err = r.persistCredentials(ctx, oob, bmctrl.Credentials())
			if err != nil {
				return nil, false, fmt.Errorf("cannot persist BMC credentials: %w", err)
			}

			// Construct a new OOB
			oob = &oobv1alpha1.OOB{
				TypeMeta: metav1.TypeMeta{
					APIVersion: oobv1alpha1.GroupVersion.String(),
					Kind:       "OOB",
				},
				ObjectMeta: metav1.ObjectMeta{
					Namespace: oob.Namespace,
					Name:      oob.Name,
				},
				Spec: oobv1alpha1.OOBSpec{
					Protocol:           oob.Spec.Protocol,
					Tags:               oob.Spec.Tags,
					Port:               oob.Spec.Port,
					PasswordExpiration: oob.Spec.PasswordExpiration,
				},
			}

			// Apply the OOB
			log.Info(ctx, "Applying OOB")
			err = r.Patch(ctx, oob, client.Apply, client.FieldOwner("oob-operator.onmetal.de/oob/creds"), client.ForceOwnership)
			if err != nil {
				return nil, false, fmt.Errorf("cannot apply OOB: %w", err)
			}

			// Delete obsolete credentials
			err = bmctrl.DeleteUsers(ctx, r.usernameRegex)
			if err != nil {
				return nil, false, fmt.Errorf("cannot delete obsolete credentials: %w", err)
			}

			return bmctrl, true, nil
		}
	}

	return bmctrl, false, nil
}

func (r *OOBReconciler) getCredentials(ctx context.Context, oob *oobv1alpha1.OOB) (bmc.Credentials, error) {
	// Get the secret
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: oob.Namespace, Name: oob.Name}, secret)
	if err != nil {
		return bmc.Credentials{}, fmt.Errorf("cannot get credentials secret: %w", err)
	}

	// Verify the secret and extract the credentials from it
	return r.getCredentialsFromSecret(secret, oob.UID, oob.APIVersion, oob.Kind, oob.Name)
}

func (r *OOBReconciler) getCredentialsFromSecret(secret *corev1.Secret, uid types.UID, apiVersion, kind, name string) (bmc.Credentials, error) {
	// Validate owner reference
	err := validateOwnerReference(secret, uid, apiVersion, kind, name)
	if err != nil {
		return bmc.Credentials{}, fmt.Errorf("credentials secret has invalid owner reference: %w", err)
	}

	// Get credentials from secret
	if secret.Type != "kubernetes.io/basic-auth" {
		return bmc.Credentials{}, fmt.Errorf("credentials secret has incorrect type: %s", secret.Type)
	}
	var username, passwd []byte
	var ok bool
	username, ok = secret.Data["username"]
	if !ok {
		return bmc.Credentials{}, fmt.Errorf("credentials secret does not contain a username")
	}
	passwd, ok = secret.Data["password"]
	if !ok {
		return bmc.Credentials{}, fmt.Errorf("credentials secret does not contain a password")
	}

	return bmc.Credentials{Username: string(username), Password: string(passwd)}, nil
}

func (r *OOBReconciler) tagMapFromK8s(tags []oobv1alpha1.TagSpec) (map[string]string, error) {
	tmap := make(map[string]string)
	for _, t := range tags {
		_, ok := tmap[t.Key]
		if ok {
			return nil, fmt.Errorf("tag keys must be unique: %s", t.Key)
		}
		tmap[t.Key] = t.Value
	}
	return tmap, nil
}

func (r *OOBReconciler) tagsToK8s(tags []tag) []oobv1alpha1.TagSpec {
	var k8sTags []oobv1alpha1.TagSpec
	for _, t := range tags {
		k8sTags = append(k8sTags, oobv1alpha1.TagSpec{Key: t.Key, Value: t.Value})
	}
	return k8sTags
}

func (r *OOBReconciler) tagMapFromTags(tags []tag) (map[string]string, error) {
	tmap := make(map[string]string)
	for _, t := range tags {
		_, ok := tmap[t.Key]
		if ok {
			return nil, fmt.Errorf("tag keys must be unique: %s", t.Key)
		}
		tmap[t.Key] = t.Value
	}
	return tmap, nil
}

func (r *OOBReconciler) createCredentials(ctx context.Context, bmctrl bmc.BMC) (time.Time, error) {
	var creds bmc.Credentials
	var err error

	// Generate credentials
	creds.Username, err = password.Generate(6, 0, 0, true, false)
	if err != nil {
		return time.Time{}, fmt.Errorf("cannot generate a random user: %w", err)
	}
	creds.Username = r.usernamePrefix + creds.Username
	creds.Password, err = password.Generate(16, 4, 0, false, false)
	if err != nil {
		return time.Time{}, fmt.Errorf("cannot generate a random password: %w", err)
	}

	// Generate a second password to be used in case of a password change requirement
	var anotherPassword string
	anotherPassword, err = password.Generate(16, 4, 0, false, false)
	if err != nil {
		return time.Time{}, fmt.Errorf("cannot generate a random password: %w", err)
	}

	// Use the existing credentials to create a new user with a new password
	var exp time.Time
	exp, err = bmctrl.CreateUser(ctx, creds, anotherPassword)
	if err != nil {
		return time.Time{}, fmt.Errorf("cannot create user: %w", err)
	}

	return exp, nil
}

func (r *OOBReconciler) persistCredentials(ctx context.Context, oob *oobv1alpha1.OOB, creds bmc.Credentials) error {
	// Construct a new secret
	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      oob.Name,
			Namespace: oob.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         oob.APIVersion,
					Kind:               oob.Kind,
					Name:               oob.Name,
					UID:                oob.UID,
					Controller:         &(&struct{ x bool }{true}).x,
					BlockOwnerDeletion: &(&struct{ x bool }{true}).x,
				},
			},
		},
		Type: "kubernetes.io/basic-auth",
		StringData: map[string]string{
			"username": creds.Username,
			"password": creds.Password,
		},
	}

	// Apply the secret
	log.Info(ctx, "Applying credentials secret")
	err := r.Patch(ctx, secret, client.Apply, client.FieldOwner("oob-operator.onmetal.de/oob/creds"), client.ForceOwnership)
	if err != nil {
		return fmt.Errorf("cannot apply secret: %w", err)
	}

	return nil
}

func (r *OOBReconciler) ensureCorrectUUIDandName(ctx context.Context, oob *oobv1alpha1.OOB, uuid string, creds bmc.Credentials) (bool, error) {
	// If the UUID changed, remove any preexisting BMCs with the same UUID
	if oob.Spec.UUID != uuid {
		if oob.Spec.UUID != "" {
			err := r.deleteOtherOOBsWithUuid(ctx, oob.Namespace, uuid, oob.UID)
			if err != nil {
				return false, fmt.Errorf("cannot clear existing OOBs with UUID %s: %w", uuid, err)
			}
		}

		// Construct a new OOB
		oob = &oobv1alpha1.OOB{
			TypeMeta: metav1.TypeMeta{
				APIVersion: oobv1alpha1.GroupVersion.String(),
				Kind:       "OOB",
			},
			ObjectMeta: metav1.ObjectMeta{
				Namespace: oob.Namespace,
				Name:      oob.Name,
			},
			Spec: oobv1alpha1.OOBSpec{
				UUID: uuid,
			},
		}

		// Apply the OOB
		log.Info(ctx, "Applying OOB")
		err := r.Patch(ctx, oob, client.Apply, client.FieldOwner("oob-operator.onmetal.de/oob/uuid"), client.ForceOwnership)
		if err != nil {
			return false, fmt.Errorf("cannot apply OOB: %w", err)
		}

		return true, nil
	}
	ctx = log.WithValues(ctx, "uuid", oob.Spec.UUID)

	// If the name does not match the UUID, replace the BMC with a new BMC with the correct name
	name := oob.Spec.UUID
	if oob.Name != name {
		err := r.replaceOOB(ctx, oob, name, creds)
		if err != nil {
			return false, fmt.Errorf("cannot replace OOB: %w", err)
		}

		return true, nil
	}

	return false, nil
}

func (r *OOBReconciler) deleteOtherOOBsWithUuid(ctx context.Context, namespace, uuid string, uid types.UID) error {
	var oobs oobv1alpha1.OOBList
	err := r.List(ctx, &oobs, client.InNamespace(namespace), client.MatchingFields{".spec.uuid": uuid})
	if err != nil {
		return fmt.Errorf("cannot list existing OOBs with the same UUID: %w", err)
	}

	for i := range oobs.Items {
		if oobs.Items[i].UID == uid {
			continue
		}

		log.Info(ctx, "Deleting existing OOB with the same UUID", "bmc", &oobs.Items[i].Name)
		err = r.Delete(ctx, &oobs.Items[i])
		if err != nil {
			return fmt.Errorf("cannot delete OOB: %w", err)
		}
	}

	return nil
}

func (r *OOBReconciler) replaceOOB(ctx context.Context, oob *oobv1alpha1.OOB, name string, creds bmc.Credentials) error {
	// Delete the obsolete BMC, this will also delete any associated secret
	log.Info(ctx, "Deleting OOB")
	err := r.Delete(ctx, oob)
	if err != nil {
		return fmt.Errorf("cannot delete OOB: %w", err)
	}

	// Construct a new OOB that a new secret can point to
	// The oob-ignore annotation prevents reconciling
	oobRepl := &oobv1alpha1.OOB{
		TypeMeta: metav1.TypeMeta{
			APIVersion: oobv1alpha1.GroupVersion.String(),
			Kind:       "OOB",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   oob.Namespace,
			Name:        name,
			Annotations: map[string]string{"oob-operator.onmetal.de/ignore": "true"},
		},
	}
	oob.Spec.DeepCopyInto(&oobRepl.Spec)
	oobRepl.Spec.UUID = ""
	ctx = log.WithValues(ctx, "newName", oobRepl.Name)

	// Apply the new OOB
	log.Info(ctx, "Applying OOB under its correct name")
	err = r.Patch(ctx, oobRepl, client.Apply, client.FieldOwner("oob-operator.onmetal.de/oob/uuid"), client.ForceOwnership)
	if err != nil {
		return fmt.Errorf("cannot apply OOB: %w", err)
	}

	// Create a new secret attached to the new OOB
	err = r.persistCredentials(ctx, oobRepl, creds)
	if err != nil {
		return fmt.Errorf("cannot create or update secret: %w", err)
	}

	// Restore the correct managedFields
	log.Info(ctx, "Patching OOB")
	oobPatch := oobRepl.DeepCopy()
	oobPatch.ManagedFields = append(oob.ManagedFields, metav1.ManagedFieldsEntry{
		Manager:    "oob-operator.onmetal.de/oob/uuid",
		Operation:  "Apply",
		APIVersion: oobv1alpha1.GroupVersion.String(),
		Time: &metav1.Time{
			Time: time.Now(),
		},
		FieldsType: "FieldsV1",
		FieldsV1: &metav1.FieldsV1{
			Raw: []byte(`{"f:metadata":{"f:annotations":{"f:oob-operator.onmetal.de/ignore":{}}}}`),
		},
	})
	err = r.Patch(ctx, oobPatch, client.MergeFrom(oobRepl))
	if err != nil {
		return fmt.Errorf("cannot patch OOB: %w", err)
	}
	oobRepl = oobPatch

	// Restore the UUID and remove the ignore annotation
	// The UUID hack is necessay in order to increment the generation number and trigger a reconciliation
	oobRepl = &oobv1alpha1.OOB{
		TypeMeta: metav1.TypeMeta{
			APIVersion: oobv1alpha1.GroupVersion.String(),
			Kind:       "OOB",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: oob.Namespace,
			Name:      name,
		},
		Spec: oobv1alpha1.OOBSpec{
			UUID: oob.Spec.UUID,
		},
	}

	// Apply the new OOB
	log.Info(ctx, "Applying OOB")
	err = r.Patch(ctx, oobRepl, client.Apply, client.FieldOwner("oob-operator.onmetal.de/oob/uuid"), client.ForceOwnership)
	if err != nil {
		return fmt.Errorf("cannot apply OOB: %w", err)
	}

	return nil
}

func (r *OOBReconciler) setNTPServers(ctx context.Context, bmctrl bmc.BMC) error {
	ntpc := bmctrl.NTPControl()
	if ntpc != nil {
		err := ntpc.SetNTPServers(ctx, r.ntpServers)
		if err != nil {
			return fmt.Errorf("cannot set NTP servers: %w", err)
		}
	}

	return nil
}

func (r *OOBReconciler) setStatusFields(oob *oobv1alpha1.OOB, info *bmc.Info, requeueAfter *time.Duration) bool {
	statusChanged := false

	// Fill in all non-modifiable fields
	if oob.Status.Manufacturer != info.Manufacturer || oob.Status.SerialNumber != info.SerialNumber || oob.Status.SKU != info.SKU {
		oob.Status.Manufacturer = info.Manufacturer
		oob.Status.SKU = info.SKU
		oob.Status.SerialNumber = info.SerialNumber
		statusChanged = true
	}

	// Update the status fields to their actual state
	if oob.Status.LocatorLED != info.LocatorLED || oob.Status.Power != info.Power || oob.Status.OSReason != info.OSReason {
		oob.Status.LocatorLED = info.LocatorLED
		oob.Status.Power = info.Power
		oob.Status.OSReason = info.OSReason
		statusChanged = true
	}

	now := metav1.Now()

	// If the machine is Off, clear OS status and all deadlines.
	if info.Power == "Off" {
		if oob.Status.OS != "" {
			oob.Status.OS = ""
			statusChanged = true
		}
		if oob.Status.OSReadDeadline != nil {
			oob.Status.OSReadDeadline = nil
			statusChanged = true
		}
		if oob.Status.ShutdownDeadline != nil {
			oob.Status.ShutdownDeadline = nil
			statusChanged = true
		}
		return statusChanged
	}

	// If the OS is Ok, clear OS read deadline.
	if info.OS == "Ok" {
		if oob.Status.OS != "Ok" {
			oob.Status.OS = "Ok"
			statusChanged = true
		}
		if oob.Status.OSReadDeadline != nil {
			oob.Status.OSReadDeadline = nil
			statusChanged = true
		}
		return statusChanged
	}

	// If there is a deadline and it has expired, set OS to TimedOut and clear the deadline.
	if !oob.Status.OSReadDeadline.IsZero() && oob.Status.OSReadDeadline.Before(&now) {
		if oob.Status.OS != "TimedOut" {
			oob.Status.OS = "TimedOut"
		}
		oob.Status.OSReadDeadline = nil
		return true
	}

	// Set OS to Waiting and set a deadline.
	if oob.Status.OS != "Waiting" {
		oob.Status.OS = "Waiting"
		statusChanged = true
	}
	if oob.Status.OSReadDeadline.IsZero() {
		oob.Status.OSReadDeadline = &metav1.Time{Time: now.Add(7 * time.Minute)}
		statusChanged = true
	}
	*requeueAfter = time.Second * 3

	return statusChanged
}

func (r *OOBReconciler) applyLocatorLED(ctx context.Context, oob *oobv1alpha1.OOB, bmctrl bmc.BMC, specChanged, statusChanged *bool) error {
	// If no change is requested or necessary, return
	if oob.Spec.LocatorLED == "" || oob.Spec.LocatorLED == oob.Status.LocatorLED {
		return nil
	}

	// If LED control is not supported, clear the request
	lc := bmctrl.LEDControl()
	if lc == nil {
		log.Info(ctx, "LED control is not supported")
		oob.Spec.LocatorLED = "None"
		*specChanged = true
		return nil
	}

	// Perform the change
	var err error
	oob.Status.LocatorLED, err = lc.SetLocatorLED(ctx, oob.Spec.LocatorLED)
	if err != nil {
		return fmt.Errorf("cannot set locator LED to %s: %w", oob.Spec.LocatorLED, err)
	}
	*statusChanged = true

	return nil
}

func (r *OOBReconciler) applyPower(ctx context.Context, oob *oobv1alpha1.OOB, bmctrl bmc.BMC, specChanged, statusChanged *bool, requeueAfter *time.Duration) error {
	// If no change is requested or necessary, return
	if oob.Spec.Power == "" || oob.Spec.Power == oob.Status.Power {
		return nil
	}

	// If power control is not supported, clear the request
	pc := bmctrl.PowerControl()
	if pc == nil {
		log.Info(ctx, "Power control is not supported")
		oob.Spec.Power = "None"
		*specChanged = true
		if oob.Status.ShutdownDeadline != nil {
			oob.Status.ShutdownDeadline = nil
			*statusChanged = true
		}
		return nil
	}

	// If a power change is requested, the action depends on both the request and the current state
	switch oob.Spec.Power {

	case "On":
		switch oob.Status.Power {
		case "On":
			// On -> On: noop

		case "Off":
			// Off -> On: turn the machine on and reconcile again after a short time to update the status
			err := pc.PowerOn(ctx)
			if err != nil {
				return fmt.Errorf("cannot power on machine: %w", err)
			}
			*requeueAfter = time.Second * 3

		default:
			return fmt.Errorf("unsupported current power state %s", oob.Status.Power)
		}

		// Clear the shutdown deadline because the machine is not shutting down
		if oob.Status.ShutdownDeadline != nil {
			oob.Status.ShutdownDeadline = nil
			*statusChanged = true
		}

	case "Reset":
		switch oob.Status.Power {
		case "On":
			// On -> Reset: reset the machine and set it to on because there is no way to monitor a reset
			err := pc.Reset(ctx, false)
			if err != nil {
				return fmt.Errorf("cannot reset machine: %w", err)
			}
			oob.Spec.Power = "On"

		case "Off":
			// Off -> Reset: set the machine to off because resetting it is a noop
			oob.Spec.Power = "Off"

		default:
			return fmt.Errorf("unsupported current power state %s", oob.Status.Power)
		}
		*specChanged = true

		// Clear the shutdown deadline because the machine is not shutting down
		if oob.Status.ShutdownDeadline != nil {
			oob.Status.ShutdownDeadline = nil
			*statusChanged = true
		}

	case "ResetImmediate":
		switch oob.Status.Power {

		case "On":
			// On -> ResetImmediate: reset the machine forcefully and set it to on because there is no way to monitor a reset
			err := pc.Reset(ctx, true)
			if err != nil {
				return fmt.Errorf("cannot reset machine: %w", err)
			}
			oob.Spec.Power = "On"

		case "Off":
			// Off -> ResetImmediate: set the machine to off because resetting it is a noop
			oob.Spec.Power = "Off"

		default:
			return fmt.Errorf("unsupported current power state %s", oob.Status.Power)
		}
		*specChanged = true

		// Clear the shutdown deadline because the machine is not shutting down
		if oob.Status.ShutdownDeadline != nil {
			oob.Status.ShutdownDeadline = nil
			*statusChanged = true
		}

	case "Off":
		switch oob.Status.Power {

		case "On":
			now := metav1.Now()

			// On -> Off: turn the machine off if it's not already shutting down, turn it off forcefully if the deadline has expired, or do nothing if it is already shutting down
			if oob.Status.ShutdownDeadline.IsZero() {
				err := pc.PowerOff(ctx, false)
				if err != nil {
					return fmt.Errorf("cannot power off machine: %w", err)
				}
				oob.Status.ShutdownDeadline = &metav1.Time{Time: now.Add(r.ShutdownTimeout)}
				*statusChanged = true
			} else if oob.Status.ShutdownDeadline.Before(&now) {
				log.Info(ctx, "Shutdown deadline exceeded, shutting down forcefully")
				err := pc.PowerOff(ctx, true)
				if err != nil {
					return fmt.Errorf("cannot power off machine: %w", err)
				}
				oob.Status.ShutdownDeadline = nil
				*statusChanged = true
			}
			*requeueAfter = time.Second * 3

		case "Off":
			// Off -> Off: noop
			// Clear the shutdown deadline because the machine is not shutting down
			if oob.Status.ShutdownDeadline != nil {
				oob.Status.ShutdownDeadline = nil
				*statusChanged = true
			}

		default:
			return fmt.Errorf("unsupported requested power state %s", oob.Spec.Power)
		}

	case "OffImmediate":
		switch oob.Status.Power {
		case "On":
			// On -> OffImmediate: turn the machine off forcefully and reconcile again after a short time to update the status
			err := pc.PowerOff(ctx, true)
			if err != nil {
				return fmt.Errorf("cannot power off machine: %w", err)
			}
			*requeueAfter = time.Second * 3

		case "Off":
			// Off -> OffImmediate: set the machine to off

		default:
			return fmt.Errorf("unsupported power state %s", oob.Status.Power)
		}

		oob.Spec.Power = "Off"
		*specChanged = true

		// Clear the shutdown deadline because the machine is not shutting down
		if oob.Status.ShutdownDeadline != nil {
			oob.Status.ShutdownDeadline = nil
			*statusChanged = true
		}

	default:
		return fmt.Errorf("unsupported power state %s", oob.Spec.Power)
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *OOBReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()

	err := mgr.GetFieldIndexer().IndexField(context.Background(), &oobv1alpha1.OOB{}, ".spec.uuid", func(obj client.Object) []string {
		oob := obj.(*oobv1alpha1.OOB)
		if oob.Spec.UUID == "" {
			return nil
		}
		return []string{oob.Spec.UUID}
	})
	if err != nil {
		return err
	}

	inCorrectNamespacePredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return r.Namespace == "" || e.Object.GetNamespace() == r.Namespace
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return r.Namespace == "" || e.ObjectNew.GetNamespace() == r.Namespace
		},
	}

	notBeingDeletedPredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return e.Object.GetDeletionTimestamp().IsZero()
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectNew.GetDeletionTimestamp().IsZero()
		},
	}

	notIgnoredPredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			ignore, ok := e.Object.GetAnnotations()["oob-operator.onmetal.de/ignore"]
			return !(ok && ignore == "true")
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			ignore, ok := e.ObjectNew.GetAnnotations()["oob-operator.onmetal.de/ignore"]
			return !(ok && ignore == "true")
		},
	}

	return ctrl.NewControllerManagedBy(mgr).For(&oobv1alpha1.OOB{}).WithEventFilter(predicate.And(predicate.GenerationChangedPredicate{}, inCorrectNamespacePredicate, notBeingDeletedPredicate, notIgnoredPredicate)).WithOptions(controller.Options{MaxConcurrentReconciles: 10}).Complete(r)
}
