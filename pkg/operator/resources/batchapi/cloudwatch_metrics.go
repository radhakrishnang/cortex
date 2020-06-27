/*
Copyright 2020 Cortex Labs, Inc.

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

package batchapi

import (
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/cloudwatch"
	"github.com/cortexlabs/cortex/pkg/lib/debug"
	"github.com/cortexlabs/cortex/pkg/lib/parallel"
	"github.com/cortexlabs/cortex/pkg/lib/slices"
	"github.com/cortexlabs/cortex/pkg/operator/config"
	"github.com/cortexlabs/cortex/pkg/types/metrics"
	"github.com/cortexlabs/cortex/pkg/types/spec"
)

func GetJobMetrics(jobSpec *spec.JobSpec) (*metrics.JobMetrics, error) {
	// Get realtime metrics for the seconds elapsed in the latest minute
	realTimeEnd := time.Now().Truncate(time.Second)
	realTimeStart := realTimeEnd.Truncate(time.Minute)

	realTimeMetrics := metrics.JobMetrics{}
	batchMetrics := metrics.JobMetrics{}
	requestList := []func() error{}

	if realTimeStart.Before(realTimeEnd) {
		requestList = append(requestList, getMetricsFunc(jobSpec, 1, &realTimeStart, &realTimeEnd, &realTimeMetrics))
	}

	batchEnd := realTimeStart
	batchStart := batchEnd.Add(-14 * 24 * time.Hour) // two weeks ago
	requestList = append(requestList, getMetricsFunc(jobSpec, 60*60, &batchStart, &batchEnd, &batchMetrics))

	err := parallel.RunFirstErr(requestList[0], requestList[1:]...)
	if err != nil {
		return nil, err
	}

	debug.Pp(realTimeMetrics)
	debug.Pp(batchMetrics)

	mergedMetrics := realTimeMetrics.Merge(batchMetrics)
	mergedMetrics.APIName = jobSpec.APIName
	mergedMetrics.JobID = jobSpec.ID
	return &mergedMetrics, nil
}

func getMetricsFunc(jobSpec *spec.JobSpec, period int64, startTime *time.Time, endTime *time.Time, metrics *metrics.JobMetrics) func() error {
	return func() error {
		metricDataResults, err := queryMetrics(jobSpec, period, startTime, endTime)
		if err != nil {
			return err
		}
		jobStats, err := extractJobStats(metricDataResults)
		if err != nil {
			return err
		}
		metrics.JobStats = jobStats

		return nil
	}
}

func queryMetrics(jobSpec *spec.JobSpec, period int64, startTime *time.Time, endTime *time.Time) ([]*cloudwatch.MetricDataResult, error) {
	allMetrics := getNetworkStatsDef(jobSpec, period)

	metricsDataQuery := cloudwatch.GetMetricDataInput{
		EndTime:           endTime,
		StartTime:         startTime,
		MetricDataQueries: allMetrics,
	}
	output, err := config.AWS.CloudWatch().GetMetricData(&metricsDataQuery)
	if err != nil {
		return nil, err
	}
	return output.MetricDataResults, nil
}

func extractJobStats(metricsDataResults []*cloudwatch.MetricDataResult) (*metrics.JobStats, error) {
	var jobStats metrics.JobStats
	var partitionCounts []*float64
	var latencyAvgs []*float64

	for _, metricData := range metricsDataResults {
		if metricData.Values == nil {
			continue
		}

		switch {
		case *metricData.Label == "Succeeded":
			jobStats.Succeeded = slices.Float64PtrSumInt(metricData.Values...)
		case *metricData.Label == "Failed":
			jobStats.Failed = slices.Float64PtrSumInt(metricData.Values...)
		case *metricData.Label == "AverageTimePerPartition":
			latencyAvgs = metricData.Values
		case *metricData.Label == "Total":
			partitionCounts = metricData.Values
		}
	}

	avg, err := slices.Float64PtrAvg(latencyAvgs, partitionCounts)
	if err != nil {
		return nil, err
	}
	jobStats.AverageTimePerPartition = avg

	jobStats.TotalCompleted = jobStats.Succeeded + jobStats.Failed
	return &jobStats, nil
}

func getAPIDimensions(jobSpec *spec.JobSpec) []*cloudwatch.Dimension {
	return []*cloudwatch.Dimension{
		{
			Name:  aws.String("APIName"),
			Value: aws.String(jobSpec.APIName),
		},
		{
			Name:  aws.String("JobID"),
			Value: aws.String(jobSpec.ID),
		},
	}
}

func getAPIDimensionsCounter(jobSpec *spec.JobSpec) []*cloudwatch.Dimension {
	return append(
		getAPIDimensions(jobSpec),
		&cloudwatch.Dimension{
			Name:  aws.String("metric_type"),
			Value: aws.String("counter"),
		},
	)
}

func getAPIDimensionsHistogram(jobSpec *spec.JobSpec) []*cloudwatch.Dimension {
	return append(
		getAPIDimensions(jobSpec),
		&cloudwatch.Dimension{
			Name:  aws.String("metric_type"),
			Value: aws.String("histogram"),
		},
	)
}

func getNetworkStatsDef(jobSpec *spec.JobSpec, period int64) []*cloudwatch.MetricDataQuery {
	return []*cloudwatch.MetricDataQuery{
		{
			Id:    aws.String("succeeded"),
			Label: aws.String("Succeeded"),
			MetricStat: &cloudwatch.MetricStat{
				Metric: &cloudwatch.Metric{
					Namespace:  aws.String(config.Cluster.LogGroup),
					MetricName: aws.String("Succeeded"),
					Dimensions: getAPIDimensionsCounter(jobSpec),
				},
				Stat:   aws.String("Sum"),
				Period: aws.Int64(period),
			},
		},
		{
			Id:    aws.String("failed"),
			Label: aws.String("Failed"),
			MetricStat: &cloudwatch.MetricStat{
				Metric: &cloudwatch.Metric{
					Namespace:  aws.String(config.Cluster.LogGroup),
					MetricName: aws.String("Failed"),
					Dimensions: getAPIDimensionsCounter(jobSpec),
				},
				Stat:   aws.String("Sum"),
				Period: aws.Int64(period),
			},
		},
		{
			Id:    aws.String("average_time_per_partition"),
			Label: aws.String("AverageTimePerPartition"),
			MetricStat: &cloudwatch.MetricStat{
				Metric: &cloudwatch.Metric{
					Namespace:  aws.String(config.Cluster.LogGroup),
					MetricName: aws.String("TimePerPartition"),
					Dimensions: getAPIDimensionsHistogram(jobSpec),
				},
				Stat:   aws.String("Average"),
				Period: aws.Int64(period),
			},
		},
		{
			Id:    aws.String("total"),
			Label: aws.String("Total"),
			MetricStat: &cloudwatch.MetricStat{
				Metric: &cloudwatch.Metric{
					Namespace:  aws.String(config.Cluster.LogGroup),
					MetricName: aws.String("TimePerPartition"),
					Dimensions: getAPIDimensionsHistogram(jobSpec),
				},
				Stat:   aws.String("SampleCount"),
				Period: aws.Int64(period),
			},
		},
	}
}