package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/emicklei/go-restful/v3"
	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	kubevirt "kubevirt.io/api/core"
	kubevirtv1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"

	"github.com/kubevirt/vm-console-proxy/api/v1alpha1"
)

const (
	AppKubernetesNameLabel      = "app.kubernetes.io/name"
	AppKubernetesPartOfLabel    = "app.kubernetes.io/part-of"
	AppKubernetesVersionLabel   = "app.kubernetes.io/version"
	AppKubernetesManagedByLabel = "app.kubernetes.io/managed-by"
	AppKubernetesComponentLabel = "app.kubernetes.io/component"
)

type Service interface {
	TokenHandler(request *restful.Request, response *restful.Response)
	VncHandler(request *restful.Request, response *restful.Response)
}

func NewService(kubevirtClient kubecli.KubevirtClient) Service {
	return &service{
		kubevirtClient: kubevirtClient,
	}
}

type service struct {
	kubevirtClient kubecli.KubevirtClient
}

func (s *service) TokenHandler(request *restful.Request, response *restful.Response) {
	params, err := readTokenRequestParameters(request)
	if err != nil {
		_ = response.WriteError(http.StatusBadRequest, err)
		return
	}

	authToken := getAuthToken(request)
	if authToken == "" {
		_ = response.WriteError(http.StatusUnauthorized, fmt.Errorf("authenticating token cannot be empty"))
		return
	}

	err = s.checkVncRbac(request.Request.Context(), authToken, params.name, params.namespace)
	if err != nil {
		_ = response.WriteError(http.StatusUnauthorized, err)
		return
	}

	vm, err := s.kubevirtClient.VirtualMachine(params.namespace).Get(params.name, &metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			_ = response.WriteError(http.StatusNotFound, fmt.Errorf("VirtualMachine does not exist: %w", err))
			return
		}
		_ = response.WriteError(http.StatusInternalServerError, fmt.Errorf("error getting VirtualMachine: %w", err))
		return
	}

	resourceName := vm.Name + "-vnc-access"
	err = s.createResources(request.Request.Context(), resourceName, vm)
	if err != nil {
		_ = response.WriteError(http.StatusInternalServerError, fmt.Errorf("error creating resources: %w", err))
		return
	}

	token, err := s.requestToken(request.Request.Context(), vm.Namespace, resourceName, params.duration)
	if err != nil {
		_ = response.WriteError(http.StatusInternalServerError, fmt.Errorf("failed to request token: %w", err))
		return
	}

	_ = response.WriteAsJson(&v1alpha1.TokenResponse{
		Token: token,
	})
}

func (s *service) VncHandler(_ *restful.Request, response *restful.Response) {
	_ = response.WriteError(http.StatusGone, fmt.Errorf(
		"/vnc endpoint was removed",
	))
}

func (s *service) checkVncRbac(ctx context.Context, rbacToken, vmiName, vmiNamespace string) error {
	tokenReview := &authnv1.TokenReview{
		Spec: authnv1.TokenReviewSpec{
			Token: rbacToken,
		},
	}

	tokenReview, err := s.kubevirtClient.AuthenticationV1().TokenReviews().Create(ctx, tokenReview, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error authenticating token: %w", err)
	}
	if tokenReview.Status.Error != "" {
		return fmt.Errorf("error authenticating token: %s", tokenReview.Status.Error)
	}

	if !tokenReview.Status.Authenticated {
		return fmt.Errorf("token is not authenticated")
	}

	extras := map[string]authzv1.ExtraValue{}
	for key, value := range tokenReview.Status.User.Extra {
		extras[key] = authzv1.ExtraValue(value)
	}

	accessReview := &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{
			ResourceAttributes: &authzv1.ResourceAttributes{
				Namespace:   vmiNamespace,
				Name:        vmiName,
				Verb:        "get",
				Group:       kubevirtv1.SubresourceGroupName,
				Version:     "v1",
				Resource:    "virtualmachineinstances",
				Subresource: "vnc",
			},
			User:   tokenReview.Status.User.Username,
			Groups: tokenReview.Status.User.Groups,
			Extra:  extras,
			UID:    tokenReview.Status.User.UID,
		},
	}

	accessReview, err = s.kubevirtClient.AuthorizationV1().SubjectAccessReviews().Create(ctx, accessReview, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error checking permissions: %w", err)
	}

	if !accessReview.Status.Allowed {
		return fmt.Errorf("does not have permission to access virtualmachineinstances/vnc endpoint: %s", accessReview.Status.Reason)
	}
	return nil
}

func (s *service) createResources(ctx context.Context, name string, vmMeta metav1.Object) error {
	const appLabelValue = "vm-console-proxy"

	namespace := vmMeta.GetNamespace()
	commonLabels := map[string]string{
		AppKubernetesNameLabel:      appLabelValue,
		AppKubernetesPartOfLabel:    appLabelValue,
		AppKubernetesManagedByLabel: appLabelValue,
		AppKubernetesComponentLabel: appLabelValue,
	}

	vmOwnerRef := metav1.OwnerReference{
		APIVersion: kubevirt.GroupName,
		Kind:       "VirtualMachine",
		Name:       vmMeta.GetName(),
		UID:        vmMeta.GetUID(),
	}

	serviceAccount, err := createOrUpdate[*core.ServiceAccount](
		ctx,
		name,
		namespace,
		s.kubevirtClient.CoreV1().ServiceAccounts(namespace),
		func(foundObj *core.ServiceAccount) {
			foundObj.Labels = commonLabels
			foundObj.OwnerReferences = []metav1.OwnerReference{vmOwnerRef}
		},
	)
	if err != nil {
		return fmt.Errorf("failed to create service account: %w", err)
	}

	role, err := createOrUpdate[*rbac.Role](
		ctx,
		name,
		namespace,
		s.kubevirtClient.RbacV1().Roles(namespace),
		func(foundObj *rbac.Role) {
			foundObj.Labels = commonLabels
			foundObj.OwnerReferences = []metav1.OwnerReference{vmOwnerRef}
			foundObj.Rules = []rbac.PolicyRule{{
				APIGroups:     []string{kubevirtv1.SubresourceGroupName},
				Resources:     []string{"virtualmachineinstances/vnc"},
				ResourceNames: []string{vmMeta.GetName()},
				Verbs:         []string{"get"},
			}}
		},
	)
	if err != nil {
		return fmt.Errorf("failed to create role: %w", err)
	}

	_, err = createOrUpdate[*rbac.RoleBinding](
		ctx,
		name,
		namespace,
		s.kubevirtClient.RbacV1().RoleBindings(namespace),
		func(foundObj *rbac.RoleBinding) {
			foundObj.Labels = commonLabels
			foundObj.OwnerReferences = []metav1.OwnerReference{vmOwnerRef}
			foundObj.Subjects = []rbac.Subject{{
				Kind: "ServiceAccount",
				Name: serviceAccount.Name,
			}}
			foundObj.RoleRef = rbac.RoleRef{
				APIGroup: rbac.GroupName,
				Kind:     "Role",
				Name:     role.Name,
			}
		},
	)
	if err != nil {
		return fmt.Errorf("failed to create role binding: %w", err)
	}
	return nil
}

func (s *service) requestToken(ctx context.Context, serviceAccountNamespace string, serviceAccountName string, duration time.Duration) (string, error) {
	durationSeconds := int64(duration.Seconds())
	tokenRequest := &authnv1.TokenRequest{
		Spec: authnv1.TokenRequestSpec{
			Audiences:         nil,
			ExpirationSeconds: &durationSeconds,
			BoundObjectRef:    nil,
		},
	}

	tokenRequest, err := s.kubevirtClient.CoreV1().ServiceAccounts(serviceAccountNamespace).CreateToken(
		ctx,
		serviceAccountName,
		tokenRequest,
		metav1.CreateOptions{},
	)
	if err != nil {
		return "", err
	}
	return tokenRequest.Status.Token, nil
}

func getAuthToken(request *restful.Request) string {
	authHeader := request.HeaderParameter("Authorization")

	const bearerPrefix = "Bearer "
	if strings.HasPrefix(authHeader, bearerPrefix) {
		return authHeader[len(bearerPrefix):]
	}
	return ""
}

type tokenRequestParams struct {
	namespace string
	name      string
	duration  time.Duration
}

func readTokenRequestParameters(request *restful.Request) (*tokenRequestParams, error) {
	namespace := request.PathParameter("namespace")
	name := request.PathParameter("name")
	if namespace == "" || name == "" {
		return nil, fmt.Errorf("namespace and name parameters are required")
	}

	duration := 10 * time.Minute
	durationParam := request.QueryParameter("duration")
	if durationParam != "" {
		var err error
		duration, err = time.ParseDuration(durationParam)
		if err != nil {
			return nil, fmt.Errorf("failed to parse duration: %w", err)
		}
	}

	return &tokenRequestParams{
		namespace: namespace,
		name:      name,
		duration:  duration,
	}, nil
}

type clientInterface[PT any] interface {
	Create(ctx context.Context, obj PT, opts metav1.CreateOptions) (PT, error)
	Update(ctx context.Context, obj PT, opts metav1.UpdateOptions) (PT, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
	Get(ctx context.Context, name string, opts metav1.GetOptions) (PT, error)
}

func createOrUpdate[PT interface {
	*T
	metav1.Object
	runtime.Object
}, T any](ctx context.Context, objName string, objNamespace string, client clientInterface[PT], mutateFn func(PT)) (PT, error) {
	return retryOnConflict(ctx, retry.DefaultRetry, func() (PT, error) {
		foundObj, err := client.Get(ctx, objName, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			newObj := PT(new(T))
			newObj.SetName(objName)
			newObj.SetNamespace(objNamespace)
			mutateFn(newObj)
			return client.Create(ctx, newObj, metav1.CreateOptions{})
		}
		if err != nil {
			return foundObj, err
		}

		copyObj := foundObj.DeepCopyObject().(PT)
		mutateFn(foundObj)

		if equality.Semantic.DeepEqual(foundObj, copyObj) {
			return foundObj, nil
		}

		return client.Update(ctx, foundObj, metav1.UpdateOptions{})
	})
}

func retryOnConflict[T any](ctx context.Context, backoff wait.Backoff, fn func() (T, error)) (T, error) {
	var result T
	var lastErr error
	err := wait.ExponentialBackoffWithContext(ctx, backoff, func() (bool, error) {
		var err error
		result, err = fn()

		switch {
		case err == nil:
			return true, nil
		case errors.IsConflict(err):
			lastErr = err
			return false, nil
		default:
			return false, err
		}
	})
	if err == wait.ErrWaitTimeout {
		return result, lastErr
	}
	return result, err
}
