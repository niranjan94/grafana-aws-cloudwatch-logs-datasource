package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/context"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"

	"github.com/grafana/grafana-plugin-model/go/datasource"
	"github.com/grafana/grafana/pkg/components/simplejson"
	plugin "github.com/hashicorp/go-plugin"
)

type AwsCloudWatchLogsDatasource struct {
	plugin.NetRPCUnsupportedPlugin
}

type Target struct {
	RefId                   string
	QueryType               string
	Format                  string
	Region                  string
	UseInsights             bool
	Input                   cloudwatchlogs.FilterLogEventsInput
	InputInsightsStartQuery cloudwatchlogs.StartQueryInput
	InputInsightsQueryId    string
	QueryId                 string
	LegendFormat            string
	TimestampColumn         string
	ValueColumn             string
	StartFromHead           bool
}

var (
	legendFormatPattern *regexp.Regexp
)

func init() {
	legendFormatPattern = regexp.MustCompile(`\{\{\s*(.+?)\s*\}\}`)
}

func (t *AwsCloudWatchLogsDatasource) Query(ctx context.Context, tsdbReq *datasource.DatasourceRequest) (*datasource.DatasourceResponse, error) {
	modelJson, err := simplejson.NewJson([]byte(tsdbReq.Queries[0].ModelJson))
	if err != nil {
		return nil, err
	}
	if modelJson.Get("queryType").MustString() == "metricFindQuery" {
		response, err := t.metricFindQuery(ctx, tsdbReq, modelJson)
		if err != nil {
			return &datasource.DatasourceResponse{
				Results: []*datasource.QueryResult{
					&datasource.QueryResult{
						RefId: "metricFindQuery",
						Error: err.Error(),
					},
				},
			}, nil
		}
		return response, nil
	}
	if modelJson.Get("queryType").MustString() == "annotationQuery" {
		target := Target{}
		if err := json.Unmarshal([]byte(tsdbReq.Queries[0].ModelJson), &target); err != nil {
			return nil, err
		}
		fromRaw, err := strconv.ParseInt(tsdbReq.TimeRange.FromRaw, 10, 64)
		if err != nil {
			return nil, err
		}
		toRaw, err := strconv.ParseInt(tsdbReq.TimeRange.ToRaw, 10, 64)
		if err != nil {
			return nil, err
		}
		target.Input.StartTime = aws.Int64(fromRaw)
		target.Input.EndTime = aws.Int64(toRaw)

		resp, err := t.getLogEvent(tsdbReq, target.Region, &target.Input, true)
		if err != nil {
			return nil, err
		}

		resultJson, err := json.Marshal(resp)
		if err != nil {
			return nil, err
		}

		return &datasource.DatasourceResponse{
			Results: []*datasource.QueryResult{
				&datasource.QueryResult{
					MetaJson: string(resultJson),
				},
			},
		}, nil
	}

	includeInsightsQuery := false
	for _, query := range tsdbReq.Queries {
		target := Target{}
		if err := json.Unmarshal([]byte(query.ModelJson), &target); err != nil {
			return nil, err
		}
		includeInsightsQuery = includeInsightsQuery || target.UseInsights
	}
	if !includeInsightsQuery {
		response, err := t.handleQuery(tsdbReq)
		if err != nil {
			return &datasource.DatasourceResponse{
				Results: []*datasource.QueryResult{
					&datasource.QueryResult{
						Error: err.Error(),
					},
				},
			}, nil
		}
		return response, nil
	} else {
		if len(tsdbReq.Queries) != 1 {
			return nil, fmt.Errorf("invalid insights query, it should be single")
		}
		response, err := t.handleInsightsQuery(tsdbReq, tsdbReq.Queries[0])
		if err != nil {
			return &datasource.DatasourceResponse{
				Results: []*datasource.QueryResult{
					&datasource.QueryResult{
						Error: err.Error(),
					},
				},
			}, nil
		}
		return response, nil
	}
}

func (t *AwsCloudWatchLogsDatasource) handleQuery(tsdbReq *datasource.DatasourceRequest) (*datasource.DatasourceResponse, error) {
	response := &datasource.DatasourceResponse{}

	fromRaw, err := strconv.ParseInt(tsdbReq.TimeRange.FromRaw, 10, 64)
	if err != nil {
		return nil, err
	}
	toRaw, err := strconv.ParseInt(tsdbReq.TimeRange.ToRaw, 10, 64)
	if err != nil {
		return nil, err
	}
	targets := make([]Target, 0)
	for _, query := range tsdbReq.Queries {
		target := Target{}
		if err := json.Unmarshal([]byte(query.ModelJson), &target); err != nil {
			return nil, err
		}
		target.Input.StartTime = aws.Int64(fromRaw)
		target.Input.EndTime = aws.Int64(toRaw)
		targets = append(targets, target)
	}

	for _, target := range targets {
		resp, err := t.getLogEvent(tsdbReq, target.Region, &target.Input, target.StartFromHead)
		if err != nil {
			return nil, err
		}

		switch target.Format {
		case "timeserie":
			return nil, fmt.Errorf("not supported")
		case "table":
			r, err := parseTableResponse(resp, target.RefId)
			if err != nil {
				return nil, err
			}
			response.Results = append(response.Results, r)
		}
	}

	return response, nil
}

func (t *AwsCloudWatchLogsDatasource) handleInsightsQuery(tsdbReq *datasource.DatasourceRequest, query *datasource.Query) (*datasource.DatasourceResponse, error) {
	response := &datasource.DatasourceResponse{}

	fromRaw, err := strconv.ParseInt(tsdbReq.TimeRange.FromRaw, 10, 64)
	if err != nil {
		return nil, err
	}
	toRaw, err := strconv.ParseInt(tsdbReq.TimeRange.ToRaw, 10, 64)
	if err != nil {
		return nil, err
	}
	target := Target{}
	if err := json.Unmarshal([]byte(query.ModelJson), &target); err != nil {
		return nil, err
	}
	target.InputInsightsStartQuery.StartTime = aws.Int64(fromRaw)
	target.InputInsightsStartQuery.EndTime = aws.Int64(toRaw)

	svc, err := t.getClient(tsdbReq.Datasource, target.Region)
	if err != nil {
		return nil, err
	}

	// start query
	if target.QueryId == "" {
		req, out := svc.StartQueryRequest(&target.InputInsightsStartQuery)
		req.RetryErrorCodes = append(req.RetryErrorCodes, cloudwatchlogs.ErrCodeLimitExceededException)
		sresp, err := out, req.Send()
		if err != nil {
			return nil, err
		}

		queryIdJson, err := json.Marshal(map[string]string{"QueryId": *sresp.QueryId})
		if err != nil {
			return nil, err
		}
		return &datasource.DatasourceResponse{
			Results: []*datasource.QueryResult{
				&datasource.QueryResult{
					RefId:    target.RefId,
					MetaJson: string(queryIdJson),
				},
			},
		}, nil
	}

	var dresp *cloudwatchlogs.DescribeQueriesOutput
	if target.InputInsightsStartQuery.LogGroupNames != nil {
		dresp, err = svc.DescribeQueries(&cloudwatchlogs.DescribeQueriesInput{LogGroupName: target.InputInsightsStartQuery.LogGroupNames[0]})
	} else {
		dresp, err = svc.DescribeQueries(&cloudwatchlogs.DescribeQueriesInput{LogGroupName: target.InputInsightsStartQuery.LogGroupName})
	}
	if err != nil {
		return nil, err
	}
	queryIndex := -1
	for i, query := range dresp.Queries {
		if *query.QueryId == target.QueryId {
			queryIndex = i
		}
	}
	if queryIndex == -1 {
		return nil, fmt.Errorf("%s is not found", target.QueryId)
	}
	if *dresp.Queries[queryIndex].Status != "Complete" {
		queryIdJson, err := json.Marshal(map[string]string{"QueryId": target.QueryId, "Status": *dresp.Queries[queryIndex].Status})
		if err != nil {
			return nil, err
		}
		return &datasource.DatasourceResponse{
			Results: []*datasource.QueryResult{
				&datasource.QueryResult{
					RefId:    target.RefId,
					MetaJson: string(queryIdJson),
				},
			},
		}, nil
	}

	gresp, err := svc.GetQueryResults(&cloudwatchlogs.GetQueryResultsInput{QueryId: aws.String(target.QueryId)})
	if err != nil {
		return nil, err
	}
	if *gresp.Status != "Complete" {
		return nil, fmt.Errorf("unexpected status")
	}

	_, err = svc.StopQuery(&cloudwatchlogs.StopQueryInput{QueryId: aws.String(target.QueryId)})
	if err != nil {
		// ignore error
	}

	queryIdJson, err := json.Marshal(map[string]string{"QueryId": target.QueryId, "Status": *gresp.Status})
	if err != nil {
		return nil, err
	}
	if len(gresp.Results) == 0 {
		return &datasource.DatasourceResponse{
			Results: []*datasource.QueryResult{
				&datasource.QueryResult{
					RefId:    target.RefId,
					MetaJson: string(queryIdJson),
				},
			},
		}, nil
	}

	if target.Format == "timeserie" {
		series := make(map[string]*datasource.TimeSeries)

		for _, r := range gresp.Results {
			var t time.Time
			var timestamp int64
			var value float64
			var err error

			kv := make(map[string]string)
			for _, d := range r {
				columnName := *d.Field
				switch columnName {
				case target.TimestampColumn:
					t, err = time.Parse("2006-01-02 15:04:05.000", *d.Value)
					if err != nil {
						return nil, err
					}
					timestamp = t.Unix() * 1000
				case target.ValueColumn:
					value, err = strconv.ParseFloat(*d.Value, 64)
					if err != nil {
						return nil, err
					}
				default:
					kv[columnName] = *d.Value
				}
			}

			name := formatLegend(kv, target.LegendFormat)
			if (series[name]) == nil {
				series[name] = &datasource.TimeSeries{
					Name: name,
					Tags: kv,
				}
			}

			series[name].Points = append(series[name].Points, &datasource.Point{
				Timestamp: timestamp,
				Value:     value,
			})
		}

		s := make([]*datasource.TimeSeries, 0)
		for _, ss := range series {
			s = append(s, ss)
		}

		response.Results = append(response.Results, &datasource.QueryResult{
			RefId:    target.RefId,
			Series:   s,
			MetaJson: string(queryIdJson),
		})
	} else {
		table := &datasource.Table{}
		for _, f := range gresp.Results[0] {
			table.Columns = append(table.Columns, &datasource.TableColumn{Name: *f.Field})
		}
		for _, r := range gresp.Results {
			row := &datasource.TableRow{}
			for _, f := range r {
				row.Values = append(row.Values, &datasource.RowValue{Kind: datasource.RowValue_TYPE_STRING, StringValue: *f.Value})
			}
			table.Rows = append(table.Rows, row)
		}

		response.Results = append(response.Results, &datasource.QueryResult{
			RefId:    target.RefId,
			Tables:   []*datasource.Table{table},
			MetaJson: string(queryIdJson),
		})
	}

	return response, nil
}

func (t *AwsCloudWatchLogsDatasource) getLogEvent(tsdbReq *datasource.DatasourceRequest, region string, input *cloudwatchlogs.FilterLogEventsInput, startFromHead bool) (*cloudwatchlogs.FilterLogEventsOutput, error) {
	svc, err := t.getClient(tsdbReq.Datasource, region)
	if err != nil {
		return nil, err
	}

	resp := &cloudwatchlogs.FilterLogEventsOutput{}
	if *input.FilterPattern != "" || len(input.LogStreamNames) != 1 {
		err = svc.FilterLogEventsPages(input,
			func(page *cloudwatchlogs.FilterLogEventsOutput, lastPage bool) bool {
				resp.Events = append(resp.Events, page.Events...)
				if len(resp.Events) > 10000 {
					return false // safety limit
				}
				if int64(len(resp.Events)) >= *input.Limit {
					return false // should stop to next query
				}
				return !lastPage
			})
	} else {
		i := &cloudwatchlogs.GetLogEventsInput{
			StartTime:     input.StartTime,
			EndTime:       input.EndTime,
			LogGroupName:  input.LogGroupName,
			LogStreamName: input.LogStreamNames[0],
			StartFromHead: aws.Bool(startFromHead),
			Limit:         input.Limit,
		}
		err = svc.GetLogEventsPages(i,
			func(page *cloudwatchlogs.GetLogEventsOutput, lastPage bool) bool {
				for _, e := range page.Events {
					fe := &cloudwatchlogs.FilteredLogEvent{
						LogStreamName: input.LogStreamNames[0],
						IngestionTime: e.IngestionTime,
						Message:       e.Message,
						Timestamp:     e.Timestamp,
					}
					resp.Events = append(resp.Events, fe)
				}
				if len(resp.Events) > 10000 {
					return false // safety limit
				}
				if int64(len(resp.Events)) >= *input.Limit {
					return false // should stop to next query
				}
				return !lastPage
			})
	}
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func parseTableResponse(resp *cloudwatchlogs.FilterLogEventsOutput, refId string) (*datasource.QueryResult, error) {
	table := &datasource.Table{}

	table.Columns = append(table.Columns, &datasource.TableColumn{Name: "Timestamp"})
	table.Columns = append(table.Columns, &datasource.TableColumn{Name: "IngestionTime"})
	table.Columns = append(table.Columns, &datasource.TableColumn{Name: "LogStreamName"})
	table.Columns = append(table.Columns, &datasource.TableColumn{Name: "Message"})
	for _, e := range resp.Events {
		row := &datasource.TableRow{}
		timestamp := time.Unix(*e.Timestamp/1000, *e.Timestamp%1000*1000*1000).Format(time.RFC3339)
		row.Values = append(row.Values, &datasource.RowValue{Kind: datasource.RowValue_TYPE_STRING, StringValue: timestamp})
		ingestionTime := time.Unix(*e.IngestionTime/1000, *e.IngestionTime%1000*1000*1000).Format(time.RFC3339)
		row.Values = append(row.Values, &datasource.RowValue{Kind: datasource.RowValue_TYPE_STRING, StringValue: ingestionTime})
		row.Values = append(row.Values, &datasource.RowValue{Kind: datasource.RowValue_TYPE_STRING, StringValue: *e.LogStreamName})
		row.Values = append(row.Values, &datasource.RowValue{Kind: datasource.RowValue_TYPE_STRING, StringValue: *e.Message})
		table.Rows = append(table.Rows, row)
	}

	return &datasource.QueryResult{
		RefId:  refId,
		Tables: []*datasource.Table{table},
	}, nil
}

func formatLegend(kv map[string]string, legendFormat string) string {
	if legendFormat == "" {
		l := make([]string, 0)
		for k, v := range kv {
			l = append(l, fmt.Sprintf("%s=\"%s\"", k, v))
		}
		return "{" + strings.Join(l, ",") + "}"
	}

	result := legendFormatPattern.ReplaceAllFunc([]byte(legendFormat), func(in []byte) []byte {
		columnName := strings.Replace(string(in), "{{", "", 1)
		columnName = strings.Replace(columnName, "}}", "", 1)
		columnName = strings.TrimSpace(columnName)
		if val, exists := kv[columnName]; exists {
			return []byte(val)
		}

		return in
	})

	return string(result)
}

type suggestData struct {
	Text  string
	Value string
}

func (t *AwsCloudWatchLogsDatasource) metricFindQuery(ctx context.Context, tsdbReq *datasource.DatasourceRequest, parameters *simplejson.Json) (*datasource.DatasourceResponse, error) {
	region := parameters.Get("region").MustString()
	svc, err := t.getClient(tsdbReq.Datasource, region)
	if err != nil {
		return nil, err
	}

	subtype := parameters.Get("subtype").MustString()

	data := make([]suggestData, 0)
	switch subtype {
	case "log_group_names":
		prefix := parameters.Get("logGroupNamePrefix").MustString()
		param := &cloudwatchlogs.DescribeLogGroupsInput{}
		if len(prefix) > 0 {
			param.LogGroupNamePrefix = aws.String(prefix)
		}
		groups := &cloudwatchlogs.DescribeLogGroupsOutput{}
		err = svc.DescribeLogGroupsPages(param, func(page *cloudwatchlogs.DescribeLogGroupsOutput, lastPage bool) bool {
			groups.LogGroups = append(groups.LogGroups, page.LogGroups...)
			if len(groups.LogGroups) > 100 {
				return false // safety limit
			}
			return !lastPage
		})
		if err != nil {
			return nil, err
		}
		sort.Slice(groups.LogGroups, func(i, j int) bool {
			return *groups.LogGroups[i].CreationTime > *groups.LogGroups[j].CreationTime
		})

		for _, g := range groups.LogGroups {
			data = append(data, suggestData{Text: *g.LogGroupName, Value: *g.LogGroupName})
		}
	case "log_stream_names":
		ctx := context.Background()
		logGroupName := parameters.Get("logGroupName").MustString()
		prefix := parameters.Get("logStreamNamePrefix").MustString()
		param := &cloudwatchlogs.DescribeLogStreamsInput{
			LogGroupName: aws.String(logGroupName),
		}
		if len(prefix) > 0 {
			param.LogStreamNamePrefix = aws.String(prefix)
		}
		streams := &cloudwatchlogs.DescribeLogStreamsOutput{}
		err = svc.DescribeLogStreamsPagesWithContext(ctx, param, func(page *cloudwatchlogs.DescribeLogStreamsOutput, lastPage bool) bool {
			streams.LogStreams = append(streams.LogStreams, page.LogStreams...)
			if len(streams.LogStreams) > 100 {
				return false // safety limit
			}
			return !lastPage
		})
		if err != nil {
			return nil, err
		}
		sort.Slice(streams.LogStreams, func(i, j int) bool {
			return *streams.LogStreams[i].CreationTime > *streams.LogStreams[j].CreationTime
		})

		for _, g := range streams.LogStreams {
			data = append(data, suggestData{Text: *g.LogStreamName, Value: *g.LogStreamName})
		}
	}

	table := t.transformToTable(data)
	return &datasource.DatasourceResponse{
		Results: []*datasource.QueryResult{
			&datasource.QueryResult{
				RefId:  "metricFindQuery",
				Tables: []*datasource.Table{table},
			},
		},
	}, nil
}

func (t *AwsCloudWatchLogsDatasource) transformToTable(data []suggestData) *datasource.Table {
	table := &datasource.Table{
		Columns: make([]*datasource.TableColumn, 0),
		Rows:    make([]*datasource.TableRow, 0),
	}
	table.Columns = append(table.Columns, &datasource.TableColumn{Name: "text"})
	table.Columns = append(table.Columns, &datasource.TableColumn{Name: "value"})

	for _, r := range data {
		row := &datasource.TableRow{}
		row.Values = append(row.Values, &datasource.RowValue{Kind: datasource.RowValue_TYPE_STRING, StringValue: r.Text})
		row.Values = append(row.Values, &datasource.RowValue{Kind: datasource.RowValue_TYPE_STRING, StringValue: r.Value})
		table.Rows = append(table.Rows, row)
	}
	return table
}
