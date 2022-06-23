package appdefinition

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"

	v1 "github.com/acorn-io/acorn/pkg/apis/internal.acorn.io/v1"
	"github.com/acorn-io/acorn/pkg/certs"
	"github.com/acorn-io/acorn/pkg/condition"
	"github.com/acorn-io/acorn/pkg/labels"
	"github.com/acorn-io/baaah/pkg/router"
	"github.com/acorn-io/baaah/pkg/typed"
	"github.com/rancher/wrangler/pkg/data/convert"
	"github.com/rancher/wrangler/pkg/merr"
	"github.com/rancher/wrangler/pkg/randomtoken"
	"golang.org/x/exp/maps"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kclient "sigs.k8s.io/controller-runtime/pkg/client"
)

func seedData(exising *corev1.Secret, from map[string]string, keys ...string) map[string][]byte {
	to := map[string][]byte{}
	if exising != nil {
		for _, key := range keys {
			to[key] = exising.Data[key]
		}
	}
	for _, key := range keys {
		if v, ok := from[key]; ok {
			// don't override a non-zero length value with zero length
			if len(v) > 0 || len(to[key]) == 0 {
				to[key] = []byte(v)
			}
		}
	}
	return to
}

var (
	ErrJobNotDone        = errors.New("job not complete")
	ErrJobNoOutput       = errors.New("job has no output")
	templateSecretRegexp = regexp.MustCompile(`\${secret://(.*?)/(.*?)}`)
)

func getCronJobLatestJob(req router.Request, namespace, name string) (jobName string, err error) {
	cronJob := &batchv1.CronJob{}
	err = req.Get(cronJob, namespace, name)
	if err != nil {
		return "", err
	}

	l := klabels.SelectorFromSet(cronJob.Spec.JobTemplate.Labels)
	if err != nil {
		return "", err
	}

	var jobsFromCron batchv1.JobList
	err = req.List(&jobsFromCron, &kclient.ListOptions{
		Namespace:     namespace,
		LabelSelector: l,
	})
	if err != nil {
		return "", err
	}

	for _, job := range jobsFromCron.Items {
		if job.Status.CompletionTime != nil && job.Status.CompletionTime.Time == cronJob.Status.LastSuccessfulTime.Time {
			jobName = job.Name
			break
		}
	}
	return
}

func getJobOutput(req router.Request, appInstance *v1.AppInstance, name string) (job *batchv1.Job, data []byte, err error) {
	namespace := appInstance.Status.Namespace

	if val, ok := appInstance.Status.AppSpec.Jobs[name]; ok {
		if val.Schedule != "" {
			name, err = getCronJobLatestJob(req, namespace, name)
			if err != nil {
				return nil, nil, err
			}
		}
	}

	job = &batchv1.Job{}
	err = req.Get(job, namespace, name)
	if err != nil {
		return nil, nil, err
	}

	if job.Status.Succeeded != 1 {
		return nil, nil, ErrJobNotDone
	}

	sel, err := metav1.LabelSelectorAsSelector(job.Spec.Selector)
	if err != nil {
		return nil, nil, err
	}

	pods := &corev1.PodList{}
	err = req.List(pods, &kclient.ListOptions{
		Namespace:     namespace,
		LabelSelector: sel,
	})
	if err != nil {
		return nil, nil, err
	}

	if len(pods.Items) == 0 {
		return nil, nil, apierrors.NewNotFound(schema.GroupResource{
			Resource: "pods",
		}, "")
	}

	for _, pod := range pods.Items {
		for _, status := range pod.Status.ContainerStatuses {
			if status.State.Terminated == nil || status.State.Terminated.ExitCode != 0 {
				continue
			}
			if len(status.State.Terminated.Message) > 0 {
				return job, []byte(status.State.Terminated.Message), nil
			}
		}
	}

	return nil, nil, ErrJobNoOutput
}

func generatedSecret(req router.Request, appInstance *v1.AppInstance, secretName string, secretRef v1.Secret, existing *corev1.Secret) (*corev1.Secret, error) {
	_, output, err := getJobOutput(req, appInstance, convert.ToString(secretRef.Params["job"]))

	if err != nil {
		return nil, err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: secretName + "-",
			Namespace:    appInstance.Namespace,
			Labels:       labelsForSecret(secretName, appInstance),
		},
		Data: seedData(existing, secretRef.Data),
		Type: "Opaque",
	}

	format := convert.ToString(secretRef.Params["format"])
	switch format {
	case "text":
		secret.Data["content"] = output
	case "json":
		newSecret := &secretData{}
		if err := json.Unmarshal(output, newSecret); err != nil {
			return nil, err
		}
		for k, v := range newSecret.Data {
			secret.Data[k] = []byte(v)
		}
		if newSecret.Type != "" {
			secret.Type = corev1.SecretType(newSecret.Type)
		}
	}

	return updateOrCreate(req, existing, secret)
}

type secretData struct {
	Type string            `json:"type,omitempty"`
	Data map[string]string `json:"data,omitempty"`
}

func generateSSH(req router.Request, appInstance *v1.AppInstance, secretName string, secretRef v1.Secret, existing *corev1.Secret) (*corev1.Secret, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: secretName + "-",
			Namespace:    appInstance.Namespace,
			Labels:       labelsForSecret(secretName, appInstance),
		},
		Data: seedData(existing, secretRef.Data, corev1.SSHAuthPrivateKey),
		Type: corev1.SecretTypeSSHAuth,
	}

	if len(secret.Data[corev1.SSHAuthPrivateKey]) == 0 {
		params := v1.TLSParams{}
		if err := convert.ToObj(secretRef.Params, &params); err != nil {
			return nil, err
		}
		params.Complete()

		key, err := certs.GeneratePrivateKey(params.Algorithm)
		if err != nil {
			return nil, err
		}

		secret.Data[corev1.SSHAuthPrivateKey] = key
	}

	return updateOrCreate(req, existing, secret)
}

func generateTemplate(secrets map[string]*corev1.Secret, req router.Request, appInstance *v1.AppInstance, secretName string, secretRef v1.Secret, existing *corev1.Secret) (*corev1.Secret, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: secretName + "-",
			Namespace:    appInstance.Namespace,
			Labels:       labelsForSecret(secretName, appInstance),
		},
		Data: seedData(existing, secretRef.Data, "template"),
		Type: "secrets.acorn.io/template",
	}

	var (
		template       = string(secret.Data["template"])
		templateErrors []error
	)
	template = templateSecretRegexp.ReplaceAllStringFunc(template, func(t string) string {
		groups := templateSecretRegexp.FindStringSubmatch(t)
		secret, err := getOrCreateSecret(secrets, req, appInstance, groups[1])
		if err != nil {
			templateErrors = append(templateErrors, err)
			return err.Error()
		}

		val := secret.Data[groups[2]]
		if len(val) == 0 {
			err := fmt.Errorf("failed to find key %s in secret %s", groups[2], groups[1])
			templateErrors = append(templateErrors, err)
			return err.Error()
		}

		return string(val)
	})

	if err := merr.NewErrors(templateErrors...); err != nil {
		return nil, err
	}

	secret.Data["template"] = []byte(template)
	return updateOrCreate(req, existing, secret)
}

func generateTLS(secrets map[string]*corev1.Secret, req router.Request, appInstance *v1.AppInstance, secretName string, secretRef v1.Secret, existing *corev1.Secret) (*corev1.Secret, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: secretName + "-",
			Namespace:    appInstance.Namespace,
			Labels:       labelsForSecret(secretName, appInstance),
		},
		Data: seedData(existing, secretRef.Data, corev1.TLSCertKey, corev1.TLSPrivateKeyKey, "ca.crt", "ca.key"),
		Type: corev1.SecretTypeTLS,
	}

	params := v1.TLSParams{}
	if err := convert.ToObj(secretRef.Params, &params); err != nil {
		return nil, err
	}

	var (
		err             error
		caPEM, caKeyPEM = secret.Data["ca.crt"], secret.Data["ca.key"]
	)

	if len(secret.Data[corev1.TLSCertKey]) == 0 || len(secret.Data[corev1.TLSPrivateKeyKey]) == 0 {
		if len(caPEM) == 0 || len(caKeyPEM) == 0 {
			if params.CASecret == "" {
				caPEM, caKeyPEM, err = certs.GenerateCA(params.Algorithm)
				if err != nil {
					return nil, err
				}
			} else {
				caSecret, err := getOrCreateSecret(secrets, req, appInstance, params.CASecret)
				if err != nil {
					return nil, err
				}
				caPEM, caKeyPEM = caSecret.Data["ca.crt"], caSecret.Data["ca.key"]
			}
		}

		cert, key, err := certs.GenerateCert(caPEM, caKeyPEM, params)
		if err != nil {
			return nil, err
		}

		secret.Data[corev1.TLSCertKey] = cert
		secret.Data[corev1.TLSPrivateKeyKey] = key
	}

	if params.CASecret == "" {
		secret.Data["ca.crt"] = caPEM
		secret.Data["ca.key"] = caKeyPEM
	}

	return updateOrCreate(req, existing, secret)
}

func generateToken(req router.Request, appInstance *v1.AppInstance, secretName string, secretRef v1.Secret, existing *corev1.Secret) (*corev1.Secret, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: secretName + "-",
			Namespace:    appInstance.Namespace,
			Labels:       labelsForSecret(secretName, appInstance),
		},
		Data: seedData(existing, secretRef.Data, "token"),
		Type: "secrets.acorn.io/token",
	}

	if len(secret.Data["token"]) == 0 {
		length, err := convert.ToNumber(secretRef.Params["length"])
		if err != nil {
			return nil, err
		}
		characters := convert.ToString(secretRef.Params["characters"])
		v, err := generate(characters, int(length))
		if err != nil {
			return nil, err
		}
		secret.Data["token"] = []byte(v)
	}

	return updateOrCreate(req, existing, secret)
}

func generateOpaque(req router.Request, appInstance *v1.AppInstance, secretName string, secretRef v1.Secret, existing *corev1.Secret) (*corev1.Secret, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: secretName + "-",
			Namespace:    appInstance.Namespace,
			Labels:       labelsForSecret(secretName, appInstance),
		},
		Data: seedData(existing, secretRef.Data, maps.Keys(secretRef.Data)...),
		Type: corev1.SecretTypeOpaque,
	}

	return updateOrCreate(req, existing, secret)
}

func generateBasic(req router.Request, appInstance *v1.AppInstance, secretName string, secretRef v1.Secret, existing *corev1.Secret) (*corev1.Secret, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: secretName + "-",
			Namespace:    appInstance.Namespace,
			Labels:       labelsForSecret(secretName, appInstance),
		},
		Data: seedData(existing, secretRef.Data, corev1.BasicAuthUsernameKey, corev1.BasicAuthPasswordKey),
		Type: corev1.SecretTypeBasicAuth,
	}

	for i, key := range []string{corev1.BasicAuthUsernameKey, corev1.BasicAuthPasswordKey} {
		if len(secret.Data[key]) == 0 {
			// TODO: Improve with more characters (special, upper/lowercase, etc)
			v, err := randomtoken.Generate()
			v = v[:(i+1)*8]
			if err != nil {
				return nil, err
			}
			secret.Data[key] = []byte(v)
		}
	}

	return updateOrCreate(req, existing, secret)
}

func updateOrCreate(req router.Request, existing, secret *corev1.Secret) (*corev1.Secret, error) {
	if existing == nil {
		return secret, req.Client.Create(req.Ctx, secret)
	}
	if equality.Semantic.DeepEqual(existing.Data, secret.Data) {
		return existing, nil
	}
	newSecret := existing.DeepCopy()
	newSecret.Data = secret.Data
	return newSecret, req.Client.Update(req.Ctx, newSecret)
}

func generateDocker(req router.Request, appInstance *v1.AppInstance, name string, secretRef v1.Secret, existing *corev1.Secret) (*corev1.Secret, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: name + "-",
			Namespace:    appInstance.Namespace,
			Labels:       labelsForSecret(name, appInstance),
		},
		Data: seedData(existing, secretRef.Data, corev1.DockerConfigJsonKey),
		Type: corev1.SecretTypeDockerConfigJson,
	}

	if len(secret.Data[corev1.DockerConfigJsonKey]) == 0 {
		secret.Data[corev1.DockerConfigJsonKey] = []byte("{}")
	}
	return updateOrCreate(req, existing, secret)
}

func labelsForSecret(secretName string, appInstance *v1.AppInstance) map[string]string {
	return map[string]string{
		labels.AcornAppName:         appInstance.Name,
		labels.AcornAppNamespace:    appInstance.Namespace,
		labels.AcornManaged:         "true",
		labels.AcornAppUID:          string(appInstance.UID),
		labels.AcornSecretName:      secretName,
		labels.AcornSecretGenerated: "true",
	}
}

func getSecret(req router.Request, appInstance *v1.AppInstance, name string) (*corev1.Secret, error) {
	l := labelsForSecret(name, appInstance)

	var secrets corev1.SecretList
	err := req.List(&secrets, &kclient.ListOptions{
		LabelSelector: klabels.SelectorFromSet(l),
	})
	if err != nil {
		return nil, err
	}

	if len(secrets.Items) == 0 {
		return nil, apierrors.NewNotFound(schema.GroupResource{
			Group:    "v1",
			Resource: "secrets",
		}, name)
	}

	sort.Slice(secrets.Items, func(i, j int) bool {
		return secrets.Items[i].UID < secrets.Items[j].UID
	})

	return &secrets.Items[0], nil
}

func generateSecret(secrets map[string]*corev1.Secret, req router.Request, appInstance *v1.AppInstance, secretName string) (*corev1.Secret, error) {
	existing, err := getSecret(req, appInstance, secretName)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	secretRef, ok := appInstance.Status.AppSpec.Secrets[secretName]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{
			Group:    "v1",
			Resource: "secrets",
		}, secretName)
	}
	switch secretRef.Type {
	case "opaque":
		return generateOpaque(req, appInstance, secretName, secretRef, existing)
	case "docker":
		return generateDocker(req, appInstance, secretName, secretRef, existing)
	case "basic":
		return generateBasic(req, appInstance, secretName, secretRef, existing)
	case "tls":
		return generateTLS(secrets, req, appInstance, secretName, secretRef, existing)
	case "ssh-auth":
		return generateSSH(req, appInstance, secretName, secretRef, existing)
	case "generated":
		return generatedSecret(req, appInstance, secretName, secretRef, existing)
	case "token":
		return generateToken(req, appInstance, secretName, secretRef, existing)
	case "template":
		return generateTemplate(secrets, req, appInstance, secretName, secretRef, existing)
	default:
		return nil, err
	}
}

func getOrCreateSecret(secrets map[string]*corev1.Secret, req router.Request, appInstance *v1.AppInstance, secretName string) (*corev1.Secret, error) {
	if sec, ok := secrets[secretName]; ok {
		return sec, nil
	}

	for _, binding := range appInstance.Spec.Secrets {
		if binding.SecretRequest == secretName {
			existingSecret := &corev1.Secret{}
			err := req.Get(existingSecret, "", binding.Secret)
			if err != nil {
				return nil, err
			}
			secrets[secretName] = existingSecret
			return existingSecret, nil
		}
	}

	secret, err := generateSecret(secrets, req, appInstance, secretName)
	if err != nil {
		return nil, err
	}
	secrets[secretName] = secret
	return secret, nil

}

type secEntry struct {
	name   string
	secret v1.Secret
}

func secretsOrdered(app *v1.AppInstance) (result []secEntry) {
	var generated []secEntry

	for _, entry := range typed.Sorted(app.Status.AppSpec.Secrets) {
		if entry.Value.Type == "generated" || entry.Value.Type == "template" {
			generated = append(generated, secEntry{name: entry.Key, secret: entry.Value})
		} else {
			result = append(result, secEntry{name: entry.Key, secret: entry.Value})
		}
	}
	return append(result, generated...)
}

func CreateSecrets(req router.Request, resp router.Response) (err error) {
	var (
		missing     []string
		errored     []string
		appInstance = req.Object.(*v1.AppInstance)
		secrets     = map[string]*corev1.Secret{}
		cond        = condition.Setter(appInstance, resp, v1.AppInstanceConditionSecrets)
	)

	defer func() {
		if err != nil {
			cond.Error(err)
			return
		}

		buf := strings.Builder{}
		if len(missing) > 0 {
			sort.Strings(missing)
			buf.WriteString("missing: [")
			buf.WriteString(strings.Join(missing, ", "))
			buf.WriteString("]")
		}
		if len(errored) > 0 {
			sort.Strings(errored)
			if buf.Len() > 0 {
				buf.WriteString(" ")
			}
			buf.WriteString("errored: [")
			buf.WriteString(strings.Join(errored, ", "))
			buf.WriteString("]")
		}

		if buf.Len() > 0 {
			cond.Error(errors.New(buf.String()))
		} else {
			cond.Success()
		}
	}()

	for _, entry := range secretsOrdered(appInstance) {
		secretName := entry.name
		secret, err := getOrCreateSecret(secrets, req, appInstance, secretName)
		if apierrors.IsNotFound(err) {
			if status := (*apierrors.StatusError)(nil); errors.As(err, &status) && status.ErrStatus.Details != nil {
				missing = append(missing, status.ErrStatus.Details.Name)
			} else {
				missing = append(missing, secretName)
			}
			continue
		} else if apiError := apierrors.APIStatus(nil); errors.As(err, &apiError) {
			cond.Error(err)
			return err
		} else if err != nil {
			errored = append(errored, fmt.Sprintf("%s: %v", secretName, err))
			continue
		}
		resp.Objects(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: appInstance.Status.Namespace,
				Labels: map[string]string{
					labels.AcornAppName:      appInstance.Name,
					labels.AcornAppNamespace: appInstance.Namespace,
					labels.AcornManaged:      "true",
				},
			},
			Data: secret.Data,
			Type: secret.Type,
		})
	}

	return nil
}

func generate(characters string, tokenLength int) (string, error) {
	token := make([]byte, tokenLength)
	for i := range token {
		r, err := rand.Int(rand.Reader, big.NewInt(int64(len(characters))))
		if err != nil {
			return "", err
		}
		token[i] = characters[r.Int64()]
	}
	return string(token), nil
}
