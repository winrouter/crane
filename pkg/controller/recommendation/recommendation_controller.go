package recommendation

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/scale"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	analysisv1alph1 "github.com/gocrane/api/analysis/v1alpha1"
	predictionapi "github.com/gocrane/api/prediction/v1alpha1"

	"github.com/gocrane/crane/pkg/prediction"
	"github.com/gocrane/crane/pkg/providers"
	"github.com/gocrane/crane/pkg/recommend"
)

const (
	RsyncPeriod           = 60 * time.Second
	ErrorFallbackPeriod   = 5 * time.Second
	DefaultTimeoutSeconds = int32(600)
)

// Controller is responsible for reconcile Recommendation
type Controller struct {
	client.Client
	ConfigSet   *analysisv1alph1.ConfigSet
	Scheme      *runtime.Scheme
	Recorder    record.EventRecorder
	RestMapper  meta.RESTMapper
	ScaleClient scale.ScalesGetter
	Predictors  map[predictionapi.AlgorithmType]prediction.Interface
	Provider    providers.Interface
}

func (c *Controller) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	klog.V(4).Infof("Got Recommendation %s", req.NamespacedName)

	recommendation := &analysisv1alph1.Recommendation{}
	err := c.Client.Get(ctx, req.NamespacedName, recommendation)
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, err
	}

	if recommendation.DeletionTimestamp != nil {
		// todo stop prediction
		return ctrl.Result{}, nil
	}

	needRecommend, needResync := c.NeedRecommend(recommendation)
	if !needRecommend {
		if needResync {
			klog.V(4).Infof("Retry recommendation after %s, Recommendation %s", RsyncPeriod, req.NamespacedName)
			return ctrl.Result{
				RequeueAfter: RsyncPeriod,
			}, err
		} else {
			klog.V(4).Infof("Nothing happens for Recommendation %s", req.NamespacedName)
			return ctrl.Result{}, nil
		}
	}

	klog.V(4).Info("Starting to process Recommendation %s", klog.KObj(recommendation))

	newStatus := recommendation.Status.DeepCopy()

	recommender, err := recommend.NewRecommender(c.Client, c.RestMapper, c.ScaleClient, recommendation, c.Predictors, c.Provider, c.ConfigSet)
	if err != nil {
		c.Recorder.Event(recommendation, v1.EventTypeNormal, "FailedCreateRecommender", err.Error())
		msg := fmt.Sprintf("Failed to create recommender, Recommendation %s error %v", klog.KObj(recommendation), err)
		klog.Errorf(msg)
		setCondition(newStatus, "Ready", metav1.ConditionFalse, "FailedCreateRecommender", msg)
		c.UpdateStatus(ctx, recommendation, newStatus)
		return ctrl.Result{}, err
	}

	proposed, err := recommender.Offer()
	if err != nil {
		c.Recorder.Event(recommendation, v1.EventTypeNormal, "FailedOfferRecommendation", err.Error())
		msg := fmt.Sprintf("Failed to offer recommend, Recommendation %s error %v", klog.KObj(recommendation), err)
		klog.Errorf(msg)
		setCondition(newStatus, "Ready", metav1.ConditionFalse, "FailedOfferRecommend", msg)
		c.UpdateStatus(ctx, recommendation, newStatus)
		return ctrl.Result{
			RequeueAfter: ErrorFallbackPeriod,
		}, err
	}

	if proposed != nil {
		newStatus.ResourceRequest = proposed.ResourceRequest
		newStatus.EffectiveHPA = proposed.EffectiveHPA
	}

	setCondition(newStatus, "Ready", metav1.ConditionTrue, "RecommendationReady", "Recommendation is ready")
	c.UpdateStatus(ctx, recommendation, newStatus)
	return ctrl.Result{}, nil
}

// NeedRecommend decide if we need do recommendation for current object
func (c *Controller) NeedRecommend(recommendation *analysisv1alph1.Recommendation) (bool, bool) {
	timeoutSeconds := DefaultTimeoutSeconds
	if recommendation.Spec.TimeoutSeconds != nil {
		timeoutSeconds = *recommendation.Spec.TimeoutSeconds
	}

	if recommendation.Spec.CompletionStrategy.CompletionStrategyType == analysisv1alph1.CompletionStrategyOnce {
		if recommendation.Status.LastSuccessfulTime != nil {
			// the recommendation is finished
			return false, false
		}

		planingRecommendTime := recommendation.CreationTimestamp.Add(time.Duration(timeoutSeconds) * time.Second)
		if time.Now().After(planingRecommendTime) {
			// timeout for CompletionStrategyOnce recommendation
			return false, false
		}
	}

	if recommendation.Spec.CompletionStrategy.CompletionStrategyType == analysisv1alph1.CompletionStrategyPeriodical {
		planingRecommendTime := recommendation.Status.LastSuccessfulTime
		if planingRecommendTime == nil {
			// the first round recommendation
			return true, false
		}

		planingRecommendTime = &metav1.Time{Time: planingRecommendTime.Add(time.Duration(*recommendation.Spec.CompletionStrategy.PeriodSeconds) * time.Second).Add(time.Duration(timeoutSeconds) * time.Second)}
		if time.Now().After(planingRecommendTime.Time) {
			// timeout for CompletionStrategyPeriodical recommendation
			// return rsync and wait next period
			return false, true
		}
	}

	return true, false
}

func (c *Controller) UpdateStatus(ctx context.Context, recommendation *analysisv1alph1.Recommendation, newStatus *analysisv1alph1.RecommendationStatus) {
	if !equality.Semantic.DeepEqual(&recommendation.Status, newStatus) {
		klog.V(4).Infof("Recommendation status should be updated, currentStatus %v newStatus %v", &recommendation.Status, newStatus)

		recommendation.Status = *newStatus
		recommendation.Status.LastUpdateTime = metav1.Now()

		var ready = false
		for _, cond := range newStatus.Conditions {
			if cond.Reason == "RecommendationReady" && cond.Status == metav1.ConditionTrue {
				ready = true
				break
			}
		}
		if ready {
			recommendation.Status.LastSuccessfulTime = &recommendation.Status.LastUpdateTime
		}

		err := c.Update(ctx, recommendation)
		if err != nil {
			c.Recorder.Event(recommendation, v1.EventTypeNormal, "FailedUpdateStatus", err.Error())
			klog.Errorf("Failed to update status, Recommendation %s error %v", klog.KObj(recommendation), err)
			return
		}

		klog.Infof("Update Recommendation status successful, Recommendation %s", klog.KObj(recommendation))
	}
}

func (c *Controller) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&analysisv1alph1.Recommendation{}).
		Complete(c)
}

func setCondition(status *analysisv1alph1.RecommendationStatus, conditionType string, conditionStatus metav1.ConditionStatus, reason string, message string) {
	for i := range status.Conditions {
		if status.Conditions[i].Type == conditionType {
			status.Conditions[i].Status = conditionStatus
			status.Conditions[i].Reason = reason
			status.Conditions[i].Message = message
			status.Conditions[i].LastTransitionTime = metav1.Now()
			return
		}
	}
	status.Conditions = append(status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             conditionStatus,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	})
}