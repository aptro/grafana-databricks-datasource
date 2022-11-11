package plugin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	_ "github.com/databricks/databricks-sql-go"
	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	"github.com/grafana/grafana-plugin-sdk-go/live"
	"regexp"
	"time"
)

// Make sure SampleDatasource implements required interfaces. This is important to do
// since otherwise we will only get a not implemented error response from plugin in
// runtime. In this example datasource instance implements backend.QueryDataHandler,
// backend.CheckHealthHandler, backend.StreamHandler interfaces. Plugin should not
// implement all these interfaces - only those which are required for a particular task.
// For example if plugin does not need streaming functionality then you are free to remove
// methods that implement backend.StreamHandler. Implementing instancemgmt.InstanceDisposer
// is useful to clean up resources used by previous datasource instance when a new datasource
// instance created upon datasource settings changed.
var (
	_                           backend.QueryDataHandler      = (*SampleDatasource)(nil)
	_                           backend.CheckHealthHandler    = (*SampleDatasource)(nil)
	_                           backend.StreamHandler         = (*SampleDatasource)(nil)
	_                           instancemgmt.InstanceDisposer = (*SampleDatasource)(nil)
	databricksConnectionsString string
	databricksDB                *sql.DB
)

// NewSampleDatasource creates a new datasource instance.
func NewSampleDatasource(settings backend.DataSourceInstanceSettings) (instancemgmt.Instance, error) {
	databricksConnectionsString = fmt.Sprintf("databricks://:%s@%s/%s", settings.DecryptedSecureJSONData["token"], settings.DecryptedSecureJSONData["hostname"], settings.DecryptedSecureJSONData["path"])
	if databricksConnectionsString != "" {
		log.DefaultLogger.Info("Init Databricks SQL DB")
		db, err := sql.Open("databricks", databricksConnectionsString)
		if err != nil {
			log.DefaultLogger.Info("DB Init Error", "err", err)
		} else {
			databricksDB = db
			log.DefaultLogger.Info("Store Databricks SQL DB Connection")
		}
	}

	return &SampleDatasource{}, nil
}

// SampleDatasource is an example datasource which can respond to data queries, reports
// its health and has streaming skills.
type SampleDatasource struct{}

// Dispose here tells plugin SDK that plugin wants to clean up resources when a new instance
// created. As soon as datasource settings change detected by SDK old datasource instance will
// be disposed and a new one will be created using NewSampleDatasource factory function.
func (d *SampleDatasource) Dispose() {
	// Clean up datasource instance resources.
}

// QueryData handles multiple queries and returns multiple responses.
// req contains the queries []DataQuery (where each query contains RefID as a unique identifier).
// The QueryDataResponse contains a map of RefID to the response for each query, and each response
// contains Frames ([]*Frame).
func (d *SampleDatasource) QueryData(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
	log.DefaultLogger.Info("QueryData called", "request", req)

	// create response struct
	response := backend.NewQueryDataResponse()

	// loop over queries and execute them individually.
	for _, q := range req.Queries {
		res := d.query(ctx, req.PluginContext, q)

		// save the response in a hashmap
		// based on with RefID as identifier
		response.Responses[q.RefID] = res
	}

	return response, nil
}

type queryModel struct {
	WithStreaming   bool   `json:"withStreaming"`
	TimeColumnName  string `json:"timeColumnName"`
	ValueColumnName string `json:"valueColumnName"`
	WhereQuery      string `json:"whereQuery"`
	TableName       string `json:"tableName"`
	RawSqlQuery     string `json:"rawSqlQuery"`
	RawSqlSelected  bool   `json:"rawSqlSelected"`
}

func (d *SampleDatasource) query(_ context.Context, pCtx backend.PluginContext, query backend.DataQuery) backend.DataResponse {
	response := backend.DataResponse{}

	// Unmarshal the JSON into our queryModel.
	var qm queryModel

	err := json.Unmarshal(query.JSON, &qm)
	if err != nil {
		response.Error = err
		log.DefaultLogger.Info("Query Parsing Error", "err", err)
		return response
	}

	seconds_interval := int64(query.Interval / 1000000000)
	log.DefaultLogger.Info("Querry Interval (sec):", seconds_interval)
	if seconds_interval <= 0 {
		seconds_interval = 1
	}
	queryString := ""
	if qm.RawSqlSelected {
		queryString = qm.RawSqlQuery
		log.DefaultLogger.Info("Raw SQL Query selected", "query", queryString)

		var rgx = regexp.MustCompile(`\$__timeWindow\(([a-zA-Z0-9_-]+)\)`)
		if rgx.MatchString(queryString) {
			log.DefaultLogger.Info("__timeWindow placeholder found")
			rs := rgx.FindStringSubmatch(queryString)
			timeColumnName := rs[1]
			queryString = rgx.ReplaceAllString(queryString, fmt.Sprintf("window(%s, '%d SECONDS')", timeColumnName, seconds_interval))

			rgx = regexp.MustCompile(`\$__time\(([a-zA-Z0-9_-]+)\)`)
			if rgx.MatchString(queryString) {
				log.DefaultLogger.Info("__time placeholder found")
				queryString = rgx.ReplaceAllString(queryString, "window.start")
			}

			rgx = regexp.MustCompile(`\$__value\(([a-zA-Z0-9_-]+)\)`)
			if rgx.MatchString(queryString) {
				log.DefaultLogger.Info("__value placeholder found")
				rs = rgx.FindStringSubmatch(queryString)
				valueColumnName := rs[1]
				queryString = rgx.ReplaceAllString(queryString, fmt.Sprintf("avg(%s) AS value", valueColumnName))
			}
		} else {
			rgx = regexp.MustCompile(`\$__time\(([a-zA-Z0-9_-]+)\)`)
			if rgx.MatchString(queryString) {
				log.DefaultLogger.Info("__time placeholder found")
				rs := rgx.FindStringSubmatch(queryString)
				timeColumnName := rs[1]
				queryString = rgx.ReplaceAllString(queryString, fmt.Sprintf("%s AS time", timeColumnName))
			}

			rgx = regexp.MustCompile(`\$__value\(([a-zA-Z0-9_-]+)\)`)
			if rgx.MatchString(queryString) {
				log.DefaultLogger.Info("__value placeholder found")
				rs := rgx.FindStringSubmatch(queryString)
				valueColumnName := rs[1]
				queryString = rgx.ReplaceAllString(queryString, fmt.Sprintf("%s AS value", valueColumnName))
			}
		}

		rgx = regexp.MustCompile(`\$__timeFilter\(([a-zA-Z0-9_-]+)\)`)
		if rgx.MatchString(queryString) {
			rs := rgx.FindStringSubmatch(queryString)
			timeColumnName := rs[1]
			timeRangeFilter := fmt.Sprintf("%s BETWEEN '%s' AND '%s'",
				timeColumnName,
				query.TimeRange.From.UTC().Format("2006-01-02 15:04:05"),
				query.TimeRange.To.UTC().Format("2006-01-02 15:04:05"),
			)
			queryString = rgx.ReplaceAllString(queryString, timeRangeFilter)
		}

	} else {
		whereQuery := ""
		if qm.WhereQuery != "" {
			whereQuery = fmt.Sprintf(" %s AND", qm.WhereQuery)
		}
		queryString = fmt.Sprintf("SELECT window.start, avg(%s) AS value FROM %s WHERE%s %s BETWEEN '%s' AND '%s' GROUP BY window(%s, '%d SECONDS')",
			qm.ValueColumnName,
			qm.TableName,
			whereQuery,
			qm.TimeColumnName,
			query.TimeRange.From.UTC().Format("2006-01-02 15:04:05"),
			query.TimeRange.To.UTC().Format("2006-01-02 15:04:05"),
			qm.TimeColumnName,
			seconds_interval)
	}
	log.DefaultLogger.Info("Query", "query", queryString)

	rows, err := databricksDB.Query(queryString)
	defer rows.Close()

	if err != nil {
		response.Error = err
		log.DefaultLogger.Info("Query Execution Error", "err", err)
		return response
	}

	rowCount := 0
	for rows.Next() {
		rowCount = rowCount + 1
	}

	rows, _ = databricksDB.Query(queryString)
	cols, err := rows.Columns() // Remember to check err afterwards

	if err != nil {
		response.Error = err
		log.DefaultLogger.Info("Extracting Columns Error", "err", err)
		return response
	}

	log.DefaultLogger.Info("Columns:", cols)
	vals := make([]interface{}, len(cols))

	timestamps := make([]time.Time, rowCount)
	values := make([]float32, rowCount)

	for i, _ := range cols {
		vals[i] = new(sql.RawBytes)
	}
	i := 0
	for rows.Next() {
		var (
			timestamp time.Time
			value     float32
		)
		err = rows.Scan(&timestamp, &value)
		if err != nil {
			response.Error = err
			log.DefaultLogger.Info("Row Scan Error", "err", err)
			return response
		}
		//log.DefaultLogger.Info("Row Next", "err", err)
		//log.DefaultLogger.Info("Returned timestamp", timestamp)
		//log.DefaultLogger.Info("Returned value", value)
		timestamps[i] = timestamp
		values[i] = value
		i = i + 1
	}
	err = rows.Err()

	if err != nil {
		response.Error = err
		log.DefaultLogger.Info("Row Error", "err", err)
		return response
	}

	// create data frame response.
	frame := data.NewFrame("response")
	frame.Fields = append(frame.Fields,
		data.NewField("timestamp", nil, timestamps),
		data.NewField(qm.ValueColumnName, nil, values),
	)
	// add fields.

	// If query called with streaming on then return a channel
	// to subscribe on a client-side and consume updates from a plugin.
	// Feel free to remove this if you don't need streaming for your datasource.
	if qm.WithStreaming {
		channel := live.Channel{
			Scope:     live.ScopeDatasource,
			Namespace: pCtx.DataSourceInstanceSettings.UID,
			Path:      "stream",
		}
		frame.SetMeta(&data.FrameMeta{Channel: channel.String()})
	}

	// add the frames to the response.
	response.Frames = append(response.Frames, frame)

	return response
}

// CheckHealth handles health checks sent from Grafana to the plugin.
// The main use case for these health checks is the test button on the
// datasource configuration page which allows users to verify that
// a datasource is working as expected.
func (d *SampleDatasource) CheckHealth(_ context.Context, req *backend.CheckHealthRequest) (*backend.CheckHealthResult, error) {
	log.DefaultLogger.Info("CheckHealth called", "request", req)

	dsn := databricksConnectionsString

	if dsn == "" {
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: "No connection string found." + "Set the DATABRICKS_DSN environment variable, and try again.",
		}, nil
	}

	//db, err := sql.Open("databricks", dsn)
	//
	//if err != nil {
	//	return &backend.CheckHealthResult{
	//		Status:  backend.HealthStatusError,
	//		Message: fmt.Sprintf("SQL Connection Failed: %s", err),
	//	}, nil
	//}

	rows, err := databricksDB.Query("SELECT 1")

	if err != nil {
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: fmt.Sprintf("SQL Connection Failed: %s", err),
		}, nil
	}

	defer rows.Close()

	return &backend.CheckHealthResult{
		Status:  backend.HealthStatusOk,
		Message: "Data source is working",
	}, nil
}

// SubscribeStream is called when a client wants to connect to a stream. This callback
// allows sending the first message.
func (d *SampleDatasource) SubscribeStream(_ context.Context, req *backend.SubscribeStreamRequest) (*backend.SubscribeStreamResponse, error) {
	log.DefaultLogger.Info("SubscribeStream called", "request", req)

	status := backend.SubscribeStreamStatusPermissionDenied
	if req.Path == "stream" {
		// Allow subscribing only on expected path.
		status = backend.SubscribeStreamStatusOK
	}
	return &backend.SubscribeStreamResponse{
		Status: status,
	}, nil
}

// RunStream is called once for any open channel.  Results are shared with everyone
// subscribed to the same channel.
func (d *SampleDatasource) RunStream(ctx context.Context, req *backend.RunStreamRequest, sender *backend.StreamSender) error {
	log.DefaultLogger.Info("RunStream called", "request", req)

	// Create the same data frame as for query data.
	frame := data.NewFrame("response")

	// Add fields (matching the same schema used in QueryData).
	frame.Fields = append(frame.Fields,
		data.NewField("time", nil, make([]time.Time, 1)),
		data.NewField("values", nil, make([]int64, 1)),
	)

	counter := 0

	// Stream data frames periodically till stream closed by Grafana.
	for {
		select {
		case <-ctx.Done():
			log.DefaultLogger.Info("Context done, finish streaming", "path", req.Path)
			return nil
		case <-time.After(time.Second):
			// Send new data periodically.
			frame.Fields[0].Set(0, time.Now())
			frame.Fields[1].Set(0, int64(10*(counter%2+1)))

			counter++

			err := sender.SendFrame(frame, data.IncludeAll)
			if err != nil {
				log.DefaultLogger.Error("Error sending frame", "error", err)
				continue
			}
		}
	}
}

// PublishStream is called when a client sends a message to the stream.
func (d *SampleDatasource) PublishStream(_ context.Context, req *backend.PublishStreamRequest) (*backend.PublishStreamResponse, error) {
	log.DefaultLogger.Info("PublishStream called", "request", req)

	// Do not allow publishing at all.
	return &backend.PublishStreamResponse{
		Status: backend.PublishStreamStatusPermissionDenied,
	}, nil
}
