package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/kubescape/kubeenforcer/pkg/alertmanager"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/klog/v2"
)

var logger klog.Logger = klog.LoggerWithName(klog.Background(), "webhook")

type Interface interface {

	// Runs the webhook server until the passed context is cancelled, or it
	// experiences an internal error.
	//
	// Error is always non-nil and will always be one of:
	//		deadline exceeded
	//		context cancelled
	//		or http listen error
	Run(ctx context.Context) error
}

func New(addr string, certFile, keyFile string, alertmanagerHost string, scheme *runtime.Scheme, validator admission.ValidationInterface) Interface {
	codecs := serializer.NewCodecFactory(scheme)
	return &webhook{
		objectInferfaces: admission.NewObjectInterfacesFromScheme(scheme),
		decoder:          codecs.UniversalDeserializer(),
		validator:        validator,
		addr:             addr,
		certFile:         certFile,
		keyFile:          keyFile,
		alertmanagerHost: alertmanagerHost,
	}
}

type webhook struct {
	lock              sync.Mutex
	port              int
	validator         admission.ValidationInterface
	objectInferfaces  admission.ObjectInterfaces
	decoder           runtime.Decoder
	addr              string
	alertmanagerHost  string
	certFile, keyFile string
}

func notifyChanges(ctx context.Context, paths ...string) <-chan struct{} {

	type info struct {
		modTime time.Time
		err     string
	}
	infos := map[string]info{}
	getInfos := func() map[string]info {
		res := map[string]info{}
		for _, v := range paths {
			fileInfo, err := os.Stat(v)
			if err != nil {
				infos[v] = info{err: err.Error()}
			} else {
				infos[v] = info{modTime: fileInfo.ModTime()}
			}

		}
		return res
	}
	lastInfos := getInfos()

	res := make(chan struct{})
	go func() {
		defer close(res)

		for {
			select {
			case <-ctx.Done():
				// context cancelled, stop watching
				return

			case <-time.After(2 * time.Second):
				newInfos := getInfos()
				if reflect.DeepEqual(lastInfos, newInfos) {
					continue
				}

				lastInfos = newInfos

				// skip event if client has not read last change
				select {
				case res <- struct{}{}:
				default:
				}
			}
		}
	}()
	return res
}

func (wh *webhook) Run(ctx context.Context) error {
	var serverError error
	var wg sync.WaitGroup

	logger.Info("starting webhook HTTP server")
	defer logger.Info("stopped webhook HTTP server")
	defer wg.Wait()

	wg.Add(1)
	defer wg.Done()

	launchServer := func() (*http.Server, <-chan error) {
		mux := http.NewServeMux()
		mux.HandleFunc("/health", wh.handleHealth)
		mux.HandleFunc("/validate", wh.handleWebhookValidate)
		srv := &http.Server{}
		srv.Handler = mux
		srv.Addr = wh.addr

		errChan := make(chan error)

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(errChan)

			err := srv.ListenAndServeTLS(wh.certFile, wh.keyFile)
			errChan <- err
			// ListenAndServeTLS always returns non-nil error
		}()

		return srv, errChan
	}

	watchCtx, cancelWatches := context.WithCancel(ctx)
	defer cancelWatches()

	keyWatch := notifyChanges(watchCtx, wh.certFile, wh.keyFile)

	currentServer, currentErrorChannel := launchServer()
loop:
	for {
		select {
		case <-ctx.Done():
			// If the caller closed their context, rather than the server having errored,
			// close the server. srv.Close() is safe to call on an already-closed server
			//
			// note: should we prefer to use Shutdown with a deadline for graceful close
			// rather than Close?
			if err := currentServer.Close(); err != nil {
				// Errors with gracefully shutting down connections. Not fatal. Server
				// is still closed.
				logger.Error(err, "shutting down webhook")
			}
			serverError = ctx.Err()
			break loop
		case serverError, _ = <-currentErrorChannel:
			// Server was closed independently of being restarted
			break loop

		case _, ok := <-keyWatch:
			if !ok {
				serverError = watchCtx.Err()
				break loop
			}

			logger.Info("TLS input has changed, restarting HTTP server")

			// Graceful shutdown, ignore any errors
			wg.Add(1)

			q := currentServer
			go func() {
				defer wg.Done()

				//!TOOD: add shutdown timeout, requests to a webhook should
				// not be long-lived
				shutdownCtx, shutdownCancel := context.WithTimeout(watchCtx, 5*time.Second)
				defer shutdownCancel()

				q.Shutdown(shutdownCtx)
			}()
			currentServer, currentErrorChannel = launchServer()
		}
	}
	return serverError
}

func (wh *webhook) handleHealth(w http.ResponseWriter, req *http.Request) {
	fmt.Fprint(w, "OK")
}

func (wh *webhook) handleWebhookValidate(w http.ResponseWriter, req *http.Request) {
	parsed, err := parseRequest(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	logger.Info(
		"review request",
		"user",
		parsed.Request.UserInfo.String(),
		"resource",
		parsed.Request.Resource.String(),
		"operation",
		parsed.Request.Operation,
		"uid",
		parsed.Request.UID,
	)

	failure := func(err error, status int) {
		http.Error(w, err.Error(), status)
		logger.Error(err, "review response", "uid", parsed.Request.UID, "status", status)
	}

	err = nil

	var attrs admission.Attributes

	if wh.validator.Handles(admission.Operation(parsed.Request.Operation)) {
		var object runtime.Object
		var oldObject runtime.Object

		if len(parsed.Request.OldObject.Raw) > 0 {
			obj, gvk, err := wh.decoder.Decode(parsed.Request.OldObject.Raw, nil, nil)
			switch {
			case gvk == nil || *gvk != schema.GroupVersionKind(parsed.Request.Kind):
				// GVK case first. If object type is unknown it is parsed to
				// unstructured, but
				failure(fmt.Errorf("unexpected GVK %v. Expected %v", gvk, parsed.Request.Kind), http.StatusBadRequest)
				return
			case err != nil && runtime.IsNotRegisteredError(err):
				var oldUnstructured unstructured.Unstructured
				err = json.Unmarshal(parsed.Request.OldObject.Raw, &oldUnstructured)
				if err != nil {
					failure(err, http.StatusInternalServerError)
					return
				}

				oldObject = &oldUnstructured
			case err != nil:
				failure(err, http.StatusBadRequest)
				return
			default:
				oldObject = obj
			}
		}

		if len(parsed.Request.Object.Raw) > 0 {
			obj, gvk, err := wh.decoder.Decode(parsed.Request.Object.Raw, nil, nil)
			switch {
			case gvk == nil || *gvk != schema.GroupVersionKind(parsed.Request.Kind):
				// GVK case first. If object type is unknown it is parsed to
				// unstructured, but
				failure(fmt.Errorf("unexpected GVK %v. Expected %v", gvk, parsed.Request.Kind), http.StatusBadRequest)
				return
			case err != nil && runtime.IsNotRegisteredError(err):
				var objUnstructured unstructured.Unstructured
				err = json.Unmarshal(parsed.Request.Object.Raw, &objUnstructured)
				if err != nil {
					failure(err, http.StatusInternalServerError)
					return
				}

				object = &objUnstructured
			case err != nil:
				failure(err, http.StatusBadRequest)
				return
			default:
				object = obj
			}
		}

		// Parse into native types if possible
		convertExtra := func(input map[string]authenticationv1.ExtraValue) map[string][]string {
			if input == nil {
				return nil
			}

			res := map[string][]string{}
			for k, v := range input {
				var converted []string
				for _, s := range v {
					converted = append(converted, string(s))
				}
				res[k] = converted
			}
			return res
		}

		//!TODO: Parse options as v1.CreateOptions, v1.DeleteOptions, or v1.PatchOptions

		attrs = admission.NewAttributesRecord(
			object,
			oldObject,
			schema.GroupVersionKind(parsed.Request.Kind),
			parsed.Request.Namespace,
			parsed.Request.Name,
			schema.GroupVersionResource{
				Group:    parsed.Request.Resource.Group,
				Version:  parsed.Request.Resource.Version,
				Resource: parsed.Request.Resource.Resource,
			},
			parsed.Request.SubResource,
			admission.Operation(parsed.Request.Operation),
			nil, // operation options?
			false,
			&user.DefaultInfo{
				Name:   parsed.Request.UserInfo.Username,
				UID:    parsed.Request.UserInfo.UID,
				Groups: parsed.Request.UserInfo.Groups,
				Extra:  convertExtra(parsed.Request.UserInfo.Extra),
			})

		err = wh.validator.Validate(context.TODO(), attrs, wh.objectInferfaces)
	}

	response := reviewResponse(
		parsed.Request.UID,
		err,
		wh.alertmanagerHost,
		parsed.Request.Resource.Resource,
		parsed.Request.Name,
		parsed.Request.Namespace,
		attrs,
	)

	out, err := json.Marshal(response)
	if err != nil {
		failure(err, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
	logger.Info(
		"review response",
		"resource",
		parsed.Request.Resource.String(),
		"namespace",
		parsed.Request.Namespace,
		"name",
		parsed.Request.Name,
		"allowed",
		response.Response.Allowed,
		"msg",
		response.Response.Result.Message,
		"reason",
		response.Response.Result.Reason,
		"uid",
		parsed.Request.UID,
	)
}

func getValidationAnnotations(attrs admission.Attributes) (audit bool, deny bool) {
	validationActionsPattern := `validationActions":\[(.*?)\]`
	regex, _ := regexp.Compile(validationActionsPattern)

	match := regex.FindStringSubmatch(fmt.Sprintf("%+v", attrs))
	if len(match) >= 2 {
		actions := match[1]
		audit = strings.Contains(actions, "Audit")
		deny = strings.Contains(actions, "Deny")
	}

	logger.Info("The actions are", "audit", audit, "deny", deny)

	return audit, deny
}

func getMessage(attrs admission.Attributes) (message string) {
	validationMessagePattern := `message":"(.*?)"`
	regex, _ := regexp.Compile(validationMessagePattern)

	match := regex.FindStringSubmatch(fmt.Sprintf("%+v", attrs))
	if len(match) >= 2 {
		message = match[1]
	}

	logger.Info("The message is", "message", message)

	return message
}

func getPolicy(attrs admission.Attributes) (policy string) {
	policyPattern := `policy":"(.*?)"`
	regex, _ := regexp.Compile(policyPattern)

	match := regex.FindStringSubmatch(fmt.Sprintf("%+v", attrs))
	if len(match) >= 2 {
		policy = match[1]
	}

	logger.Info("The policy is", "policy", policy)

	return policy
}

func reviewResponse(uid types.UID, err error, aletmanagerHost string, resource string, name string, namespace string, attrs admission.Attributes) *admissionv1.AdmissionReview {
	allowed := err == nil
	var status int32 = http.StatusAccepted
	if err != nil {
		status = http.StatusForbidden
	}
	reason := metav1.StatusReasonUnknown
	message := "valid"
	if err != nil {
		message = err.Error()
	}

	var statusErr *k8serrors.StatusError
	if ok := errors.As(err, &statusErr); ok {
		reason = statusErr.ErrStatus.Reason
		message = statusErr.ErrStatus.Message
		status = statusErr.ErrStatus.Code
	}

	audit, deny := getValidationAnnotations(attrs)
	if audit || deny {
		if aletmanagerHost != "" {
			policyName := getPolicy(attrs)
			alerter := alertmanager.New(aletmanagerHost, "")
			alertInfo := alertmanager.AlertInfo{
				Name:        fmt.Sprintf("Failed Policy: %v", policyName),
				Severity:    string(reason),
				Resource:    resource,
				Instance:    name,
				Namespace:   namespace,
				Description: getMessage(attrs),
			}
			alerter.Alert(&alertInfo)
		}
	}

	return &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			Kind:       "AdmissionReview",
			APIVersion: "admission.k8s.io/v1",
		},
		Response: &admissionv1.AdmissionResponse{
			UID:     uid,
			Allowed: allowed,
			Result: &metav1.Status{
				Code:    status,
				Message: message,
				Reason:  reason,
			},
		},
	}
}

// parseRequest extracts an AdmissionReview from an http.Request if possible
func parseRequest(r *http.Request) (*admissionv1.AdmissionReview, error) {
	if r.Header.Get("Content-Type") != "application/json" {
		return nil, fmt.Errorf("Content-Type: %q should be %q",
			r.Header.Get("Content-Type"), "application/json")
	}

	bodybuf := new(bytes.Buffer)
	bodybuf.ReadFrom(r.Body)
	body := bodybuf.Bytes()

	if len(body) == 0 {
		return nil, fmt.Errorf("admission request body is empty")
	}

	var a admissionv1.AdmissionReview

	if err := json.Unmarshal(body, &a); err != nil {
		return nil, fmt.Errorf("could not parse admission review request: %v", err)
	}

	if a.Request == nil {
		return nil, fmt.Errorf("admission review can't be used: Request field is nil")
	}

	return &a, nil
}
