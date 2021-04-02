package drivers

import (
	"context"
	"errors"
	"fmt"
	"github.com/jitsucom/jitsu/server/logging"
	"github.com/jitsucom/jitsu/server/typing"
	ga "google.golang.org/api/analyticsreporting/v4"
	"google.golang.org/api/option"
	"strings"
	"time"
)

const (
	dayLayout         = "2006-01-02"
	reportsCollection = "report"
	gaFieldsPrefix    = "ga:"
	eventID           = "event_id"

	gaMaxAttempts = 3 // sometimes Google API returns errors for unknown reasons, this is a number of retries we make before fail to get a report
)

var (
	metricsCast = map[string]func(interface{}) (interface{}, error){
		"ga:sessions":         typing.StringToInt,
		"ga:users":            typing.StringToInt,
		"ga:hits":             typing.StringToInt,
		"ga:visitors":         typing.StringToInt,
		"ga:bounces":          typing.StringToInt,
		"ga:goal1Completions": typing.StringToInt,
		"ga:goal2Completions": typing.StringToInt,
		"ga:goal3Completions": typing.StringToInt,
		"ga:goal4Completions": typing.StringToInt,
		"ga:adClicks":         typing.StringToInt,
		"ga:newUsers":         typing.StringToInt,
		"ga:pageviews":        typing.StringToInt,
		"ga:uniquePageviews":  typing.StringToInt,
		"ga:transactions":     typing.StringToInt,

		"ga:adCost":             typing.StringToFloat,
		"ga:avgSessionDuration": typing.StringToFloat,
		"ga:timeOnPage":         typing.StringToFloat,
		"ga:avgTimeOnPage":      typing.StringToFloat,
		"ga:transactionRevenue": typing.StringToFloat,
	}
)

type GoogleAnalyticsConfig struct {
	AuthConfig *GoogleAuthConfig `mapstructure:"auth" json:"auth,omitempty" yaml:"auth,omitempty"`
	ViewID     string            `mapstructure:"view_id" json:"view_id,omitempty" yaml:"view_id,omitempty"`
}

type GAReportFieldsConfig struct {
	Dimensions []string `mapstructure:"dimensions" json:"dimensions,omitempty" yaml:"dimensions,omitempty"`
	Metrics    []string `mapstructure:"metrics" json:"metrics,omitempty" yaml:"metrics,omitempty"`
}

func (gac *GoogleAnalyticsConfig) Validate() error {
	if gac.ViewID == "" {
		return fmt.Errorf("view_id field must not be empty")
	}
	return gac.AuthConfig.Validate()
}

type GoogleAnalytics struct {
	ctx                context.Context
	config             *GoogleAnalyticsConfig
	service            *ga.Service
	collection         *Collection
	reportFieldsConfig *GAReportFieldsConfig
}

func init() {
	if err := RegisterDriver(GoogleAnalyticsType, NewGoogleAnalytics); err != nil {
		logging.Errorf("Failed to register driver %s: %v", GoogleAnalyticsType, err)
	}
}

func NewGoogleAnalytics(ctx context.Context, sourceConfig *SourceConfig, collection *Collection) (Driver, error) {
	config := &GoogleAnalyticsConfig{}
	err := unmarshalConfig(sourceConfig.Config, config)
	if err != nil {
		return nil, err
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	var reportFieldsConfig GAReportFieldsConfig
	err = unmarshalConfig(collection.Parameters, &reportFieldsConfig)
	if err != nil {
		return nil, err
	}
	if len(reportFieldsConfig.Metrics) == 0 || len(reportFieldsConfig.Dimensions) == 0 {
		return nil, errors.New("metrics and dimensions must not be empty")
	}
	credentialsJSON, err := config.AuthConfig.Marshal()
	if err != nil {
		return nil, err
	}
	service, err := ga.NewService(ctx, option.WithCredentialsJSON(credentialsJSON))
	if err != nil {
		return nil, fmt.Errorf("failed to create GA service: %v", err)
	}
	return &GoogleAnalytics{ctx: ctx, config: config, collection: collection, service: service,
		reportFieldsConfig: &reportFieldsConfig}, nil
}

func (g *GoogleAnalytics) GetAllAvailableIntervals() ([]*TimeInterval, error) {
	var intervals []*TimeInterval
	daysBackToLoad := defaultDaysBackToLoad
	if g.collection.DaysBackToLoad > 0 {
		daysBackToLoad = g.collection.DaysBackToLoad
	}

	now := time.Now().UTC()
	for i := 0; i < daysBackToLoad; i++ {
		date := now.AddDate(0, 0, -i)
		intervals = append(intervals, NewTimeInterval(DAY, date))
	}
	return intervals, nil
}

func (g *GoogleAnalytics) GetObjectsFor(interval *TimeInterval) ([]map[string]interface{}, error) {
	logging.Debug("Sync time interval:", interval.String())
	dateRanges := []*ga.DateRange{
		{StartDate: interval.LowerEndpoint().Format(dayLayout),
			EndDate: interval.UpperEndpoint().Format(dayLayout)},
	}

	if g.collection.Type == reportsCollection {
		result, err := g.loadReport(g.config.ViewID, dateRanges, g.reportFieldsConfig.Dimensions, g.reportFieldsConfig.Metrics)
		logging.Debugf("[%s] Rows to sync: %d", interval.String(), len(result))
		return result, err
	}

	return nil, fmt.Errorf("Unknown collection %s: only 'report' is supported", g.collection.Type)
}

func (g *GoogleAnalytics) Type() string {
	return GoogleAnalyticsType
}

func (g *GoogleAnalytics) Close() error {
	return nil
}

func (g *GoogleAnalytics) GetCollectionTable() string {
	return g.collection.GetTableName()
}

func (g *GoogleAnalytics) TestConnection() error {
	startDate := time.Now().UTC()
	endDate := startDate.AddDate(0, 0, -1)
	req := &ga.GetReportsRequest{
		ReportRequests: []*ga.ReportRequest{
			{
				ViewId: g.config.ViewID,
				DateRanges: []*ga.DateRange{
					{StartDate: startDate.Format(dayLayout),
						EndDate: endDate.Format(dayLayout)},
				},
				//Metrics:    gaMetrics, TODO
				//Dimensions: gaDimensions,
				PageToken: "",
				PageSize:  1,
			},
		},
	}
	_, err := g.executeWithRetry(g.service.Reports.BatchGet(req))
	if err != nil {
		return err
	}

	return nil
}

func (g *GoogleAnalytics) loadReport(viewID string, dateRanges []*ga.DateRange, dimensions []string, metrics []string) ([]map[string]interface{}, error) {
	var gaDimensions []*ga.Dimension
	for _, dimension := range dimensions {
		gaDimensions = append(gaDimensions, &ga.Dimension{Name: dimension})
	}
	var gaMetrics []*ga.Metric
	for _, metric := range metrics {
		gaMetrics = append(gaMetrics, &ga.Metric{Expression: metric})
	}

	nextPageToken := ""
	var result []map[string]interface{}
	for {
		req := &ga.GetReportsRequest{
			ReportRequests: []*ga.ReportRequest{
				{
					ViewId:     viewID,
					DateRanges: dateRanges,
					Metrics:    gaMetrics,
					Dimensions: gaDimensions,
					PageToken:  nextPageToken,
					PageSize:   40000,
				},
			},
		}
		response, err := g.executeWithRetry(g.service.Reports.BatchGet(req))
		if err != nil {
			return nil, err
		}
		report := response.Reports[0]
		header := report.ColumnHeader
		dimHeaders := header.Dimensions
		metricHeaders := header.MetricHeader.MetricHeaderEntries
		rows := report.Data.Rows
		for _, row := range rows {
			gaEvent := make(map[string]interface{})
			dims := row.Dimensions
			for i := 0; i < len(dimHeaders) && i < len(dims); i++ {
				gaEvent[strings.TrimPrefix(dimHeaders[i], gaFieldsPrefix)] = dims[i]
			}

			metrics := row.Metrics
			for _, metric := range metrics {
				for j := 0; j < len(metricHeaders) && j < len(metric.Values); j++ {
					fieldName := strings.TrimPrefix(metricHeaders[j].Name, gaFieldsPrefix)
					stringValue := metric.Values[j]
					convertFunc, ok := metricsCast[metricHeaders[j].Name]
					if ok {
						convertedValue, err := convertFunc(stringValue)
						if err != nil {
							return nil, err
						}
						gaEvent[fieldName] = convertedValue
					} else {
						gaEvent[fieldName] = stringValue
					}
				}
			}
			result = append(result, gaEvent)
		}
		nextPageToken = report.NextPageToken
		if nextPageToken == "" {
			break
		}
	}

	return result, nil
}

func (g *GoogleAnalytics) executeWithRetry(reportCall *ga.ReportsBatchGetCall) (*ga.GetReportsResponse, error) {
	attempt := 0
	var response *ga.GetReportsResponse
	var err error
	for attempt < gaMaxAttempts {
		response, err = reportCall.Do()
		if err == nil {
			return response, nil
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
		attempt++
	}
	return nil, err
}
