package scalers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kedacore/keda/v2/pkg/scalers/authentication"
	"io"
	"net/http"
	"strconv"
	"strings"

	"golang.org/x/oauth2/clientcredentials"

	"github.com/go-logr/logr"
	"k8s.io/api/autoscaling/v2beta2"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/metrics/pkg/apis/external_metrics"

	kedautil "github.com/kedacore/keda/v2/pkg/util"
)

type pulsarScaler struct {
	metadata pulsarMetadata
	client   *http.Client
	logger   logr.Logger
}

type pulsarMetadata struct {
	adminURL                      string
	topic                         string
	subscription                  string
	msgBacklogThreshold           int64
	activationMsgBacklogThreshold int64

	pulsarAuth *authentication.AuthMeta

	statsURL    string
	metricName  string
	scalerIndex int
}

const (
	msgBacklogMetricName       = "msgBacklog"
	pulsarMetricType           = "External"
	defaultMsgBacklogThreshold = 10
	enable                     = "enable"
	stringTrue                 = "true"
	pulsarAuthModeHeader       = "X-Pulsar-Auth-Method-Name"
)

type pulsarSubscription struct {
	Msgrateout                       float64       `json:"msgRateOut"`
	Msgthroughputout                 float64       `json:"msgThroughputOut"`
	Bytesoutcounter                  int           `json:"bytesOutCounter"`
	Msgoutcounter                    int           `json:"msgOutCounter"`
	Msgrateredeliver                 float64       `json:"msgRateRedeliver"`
	Chuckedmessagerate               int           `json:"chuckedMessageRate"`
	Msgbacklog                       int64         `json:"msgBacklog"`
	Msgbacklognodelayed              int           `json:"msgBacklogNoDelayed"`
	Blockedsubscriptiononunackedmsgs bool          `json:"blockedSubscriptionOnUnackedMsgs"`
	Msgdelayed                       int           `json:"msgDelayed"`
	Unackedmessages                  int           `json:"unackedMessages"`
	Type                             string        `json:"type"`
	Msgrateexpired                   float64       `json:"msgRateExpired"`
	Lastexpiretimestamp              int           `json:"lastExpireTimestamp"`
	Lastconsumedflowtimestamp        int64         `json:"lastConsumedFlowTimestamp"`
	Lastconsumedtimestamp            int           `json:"lastConsumedTimestamp"`
	Lastackedtimestamp               int           `json:"lastAckedTimestamp"`
	Consumers                        []interface{} `json:"consumers"`
	Isdurable                        bool          `json:"isDurable"`
	Isreplicated                     bool          `json:"isReplicated"`
	Consumersaftermarkdeleteposition struct {
	} `json:"consumersAfterMarkDeletePosition"`
}

type pulsarStats struct {
	Msgratein         float64                       `json:"msgRateIn"`
	Msgthroughputin   float64                       `json:"msgThroughputIn"`
	Msgrateout        float64                       `json:"msgRateOut"`
	Msgthroughputout  float64                       `json:"msgThroughputOut"`
	Bytesincounter    int                           `json:"bytesInCounter"`
	Msgincounter      int                           `json:"msgInCounter"`
	Bytesoutcounter   int                           `json:"bytesOutCounter"`
	Msgoutcounter     int                           `json:"msgOutCounter"`
	Averagemsgsize    float64                       `json:"averageMsgSize"`
	Msgchunkpublished bool                          `json:"msgChunkPublished"`
	Storagesize       int                           `json:"storageSize"`
	Backlogsize       int                           `json:"backlogSize"`
	Publishers        []interface{}                 `json:"publishers"`
	Subscriptions     map[string]pulsarSubscription `json:"subscriptions"`
	Replication       struct {
	} `json:"replication"`
	Deduplicationstatus string `json:"deduplicationStatus"`
}

// NewPulsarScaler creates a new PulsarScaler
func NewPulsarScaler(config *ScalerConfig) (Scaler, error) {
	pulsarMetadata, err := parsePulsarMetadata(config)
	if err != nil {
		return nil, fmt.Errorf("error parsing pulsar metadata: %s", err)
	}

	client := kedautil.CreateHTTPClient(config.GlobalHTTPTimeout, false)

	if pulsarMetadata.pulsarAuth != nil {
		if pulsarMetadata.pulsarAuth.EnableBearerAuth || pulsarMetadata.pulsarAuth.EnableBasicAuth {
			// The pulsar broker redirects HTTP calls to other brokers and expects the Authorization header
			client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
				if len(via) != 0 && via[0].Response.StatusCode == http.StatusTemporaryRedirect {
					addAuthHeaders(req, &pulsarMetadata)
				}
				return nil
			}
		}
	}

	return &pulsarScaler{
		client:   client,
		metadata: pulsarMetadata,
		logger:   InitializeLogger(config, "pulsar_scaler"),
	}, nil
}

func parsePulsarMetadata(config *ScalerConfig) (pulsarMetadata, error) {
	meta := pulsarMetadata{}
	switch {
	case config.TriggerMetadata["adminURLFromEnv"] != "":
		meta.adminURL = config.ResolvedEnv[config.TriggerMetadata["adminURLFromEnv"]]
	case config.TriggerMetadata["adminURL"] != "":
		meta.adminURL = config.TriggerMetadata["adminURL"]
	default:
		return meta, errors.New("no adminURL given")
	}

	switch {
	case config.TriggerMetadata["topicFromEnv"] != "":
		meta.topic = config.ResolvedEnv[config.TriggerMetadata["topicFromEnv"]]
	case config.TriggerMetadata["topic"] != "":
		meta.topic = config.TriggerMetadata["topic"]
	default:
		return meta, errors.New("no topic given")
	}

	topic := strings.ReplaceAll(meta.topic, "persistent://", "")
	meta.statsURL = meta.adminURL + "/admin/v2/persistent/" + topic + "/stats"

	switch {
	case config.TriggerMetadata["subscriptionFromEnv"] != "":
		meta.subscription = config.ResolvedEnv[config.TriggerMetadata["subscriptionFromEnv"]]
	case config.TriggerMetadata["subscription"] != "":
		meta.subscription = config.TriggerMetadata["subscription"]
	default:
		return meta, errors.New("no subscription given")
	}
	meta.metricName = fmt.Sprintf("%s-%s-%s", "pulsar", meta.topic, meta.subscription)

	meta.activationMsgBacklogThreshold = 0
	if val, ok := config.TriggerMetadata["activationMsgBacklogThreshold"]; ok {
		activationMsgBacklogThreshold, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return meta, fmt.Errorf("activationMsgBacklogThreshold parsing error %s", err.Error())
		}
		meta.activationMsgBacklogThreshold = activationMsgBacklogThreshold
	}

	meta.msgBacklogThreshold = defaultMsgBacklogThreshold

	if val, ok := config.TriggerMetadata[msgBacklogMetricName]; ok {
		t, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return meta, fmt.Errorf("error parsing %s: %s", msgBacklogMetricName, err)
		}
		meta.msgBacklogThreshold = t
	}

	auth, err := authentication.GetAuthConfigs(config.TriggerMetadata, config.AuthParams)
	if err != nil {
		return meta, fmt.Errorf("error parsing %s: %w", msgBacklogMetricName, err)
	}

	if auth != nil && auth.EnableOAuth {
		if auth.OauthTokenURI == "" {
			auth.OauthTokenURI = config.TriggerMetadata["oauthTokenURI"]
		}
		if auth.Scopes == nil {
			auth.Scopes = authentication.ParseScope(config.TriggerMetadata["scope"])
		}
		if auth.ClientID == "" {
			auth.ClientID = config.TriggerMetadata["clientID"]
		}
		if auth.ClientSecret == "" {
			auth.ClientSecret = "notRequired"
		}
	}
	meta.pulsarAuth = auth
	meta.scalerIndex = config.ScalerIndex
	return meta, nil
}

func (s *pulsarScaler) GetStats(ctx context.Context) (*pulsarStats, error) {
	stats := new(pulsarStats)

	req, err := http.NewRequestWithContext(ctx, "GET", s.metadata.statsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("error requesting stats from url: %s", err)
	}

	client := s.client
	if s.metadata.pulsarAuth.EnableOAuth {
		config := clientcredentials.Config{
			ClientID:     s.metadata.pulsarAuth.ClientID,
			ClientSecret: s.metadata.pulsarAuth.ClientSecret,
			TokenURL:     s.metadata.pulsarAuth.OauthTokenURI,
			Scopes:       s.metadata.pulsarAuth.Scopes,
		}
		client = config.Client(context.Background())
	}
	addAuthHeaders(req, &s.metadata)

	res, err := client.Do(req)
	if res == nil || err != nil {
		return nil, fmt.Errorf("error requesting stats from url: %s", err)
	}

	defer res.Body.Close()

	switch res.StatusCode {
	case 200:
		body, err := io.ReadAll(res.Body)
		if err != nil {
			return nil, fmt.Errorf("error requesting stats from url: %s", err)
		}
		err = json.Unmarshal(body, stats)
		if err != nil {
			return nil, fmt.Errorf("error unmarshalling response: %s", err)
		}
		return stats, nil
	case 404:
		return nil, fmt.Errorf("error requesting stats from url: %s", err)
	default:
		return nil, fmt.Errorf("error requesting stats from url: %s", err)
	}
}

func (s *pulsarScaler) getMsgBackLog(ctx context.Context) (int64, bool, error) {
	stats, err := s.GetStats(ctx)
	if err != nil {
		return 0, false, err
	}

	if stats == nil {
		return 0, false, nil
	}

	v, found := stats.Subscriptions[s.metadata.subscription]

	return v.Msgbacklog, found, nil
}

// IsActive determines if we need to scale from zero
func (s *pulsarScaler) IsActive(ctx context.Context) (bool, error) {
	msgBackLog, found, err := s.getMsgBackLog(ctx)
	if err != nil {
		return false, err
	}

	if !found {
		s.logger.Info("Pulsar subscription is not active, either no subscription found or no backlog detected")
		return false, nil
	}

	return msgBackLog > s.metadata.activationMsgBacklogThreshold, nil
}

// GetMetrics returns value for a supported metric and an error if there is a problem getting the metric
func (s *pulsarScaler) GetMetrics(ctx context.Context, metricName string, _ labels.Selector) ([]external_metrics.ExternalMetricValue, error) {
	msgBacklog, found, err := s.getMsgBackLog(ctx)
	if err != nil {
		return nil, fmt.Errorf("error requesting stats from url: %s", err)
	}

	if !found {
		return nil, fmt.Errorf("have not subscription found! %s", s.metadata.subscription)
	}

	metric := GenerateMetricInMili(metricName, float64(msgBacklog))

	return append([]external_metrics.ExternalMetricValue{}, metric), nil
}

func (s *pulsarScaler) GetMetricSpecForScaling(context.Context) []v2beta2.MetricSpec {
	targetMetricValue := resource.NewQuantity(s.metadata.msgBacklogThreshold, resource.DecimalSI)

	externalMetric := &v2beta2.ExternalMetricSource{
		Metric: v2beta2.MetricIdentifier{
			Name: GenerateMetricNameWithIndex(s.metadata.scalerIndex, kedautil.NormalizeString(s.metadata.metricName)),
		},
		Target: v2beta2.MetricTarget{
			Type:         v2beta2.AverageValueMetricType,
			AverageValue: targetMetricValue,
		},
	}
	metricSpec := v2beta2.MetricSpec{External: externalMetric, Type: pulsarMetricType}
	return []v2beta2.MetricSpec{metricSpec}
}

func (s *pulsarScaler) Close(context.Context) error {
	s.client = nil
	return nil
}

// addAuthHeaders add the relevant headers used by Pulsar to authenticate and authorize http requests
func addAuthHeaders(req *http.Request, metadata *pulsarMetadata) {
	if metadata.pulsarAuth == nil {
		return
	}
	switch {
	case metadata.pulsarAuth.EnableBearerAuth:
		req.Header.Add("Authorization", authentication.GetBearerToken(metadata.pulsarAuth))
		req.Header.Add(pulsarAuthModeHeader, "token")
	case metadata.pulsarAuth.EnableBasicAuth:
		req.SetBasicAuth(metadata.pulsarAuth.Username, metadata.pulsarAuth.Password)
		req.Header.Add(pulsarAuthModeHeader, "basic")
	case metadata.pulsarAuth.EnableTLS:
		// When BearerAuth or BasicAuth are also configured, let them take precedence for the purposes of
		// the authMode header.
		req.Header.Add(pulsarAuthModeHeader, "tls")
	}
}
