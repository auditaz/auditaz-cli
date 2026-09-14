package awsx

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// clientOverRegions builds a Client that scans the given regions without any
// network or credential setup — mapRegions only needs the region list and a
// config entry per region.
func clientOverRegions(regions ...string) *Client {
	c := &Client{regions: regions, cfgByRegion: map[string]aws.Config{}}
	for _, r := range regions {
		c.cfgByRegion[r] = aws.Config{Region: r}
	}
	return c
}

func TestOneFailingRegionKeepsTheOthersResults(t *testing.T) {
	// Arrange: ap-northeast-2 can't be reached, the way a DNS failure behaves.
	c := clientOverRegions("us-east-1", "eu-west-1", "ap-northeast-2")

	// Act
	got, err := mapRegions(c, context.Background(), func(_ context.Context, region string, _ aws.Config) ([]string, error) {
		if region == "ap-northeast-2" {
			return nil, errors.New("dial tcp: lookup ec2.ap-northeast-2.amazonaws.com: no such host")
		}
		return []string{region + "/resource"}, nil
	})

	// Assert: the healthy regions' data survives, and the failure is reported.
	if err == nil {
		t.Fatal("the failed region must still be reported as an error")
	}
	sort.Strings(got)
	want := []string{"eu-west-1/resource", "us-east-1/resource"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v — a false zero is worse than a reported failure", got, want)
	}
	if !strings.Contains(err.Error(), "ap-northeast-2") {
		t.Errorf("error should name the region that failed: %v", err)
	}
}

func TestEveryRegionRunsEvenAfterAnEarlyFailure(t *testing.T) {
	// Arrange: the first region fails; the rest must not be cancelled.
	regions := []string{"us-east-1", "eu-west-1", "sa-east-1", "ap-south-1"}
	c := clientOverRegions(regions...)
	var visited []string
	var mu = make(chan struct{}, 1)
	mu <- struct{}{}

	// Act
	_, err := mapRegions(c, context.Background(), func(_ context.Context, region string, _ aws.Config) ([]string, error) {
		<-mu
		visited = append(visited, region)
		mu <- struct{}{}
		if region == "us-east-1" {
			return nil, errors.New("boom")
		}
		return []string{region}, nil
	})

	// Assert
	if err == nil {
		t.Fatal("expected the failure to be reported")
	}
	if len(visited) != len(regions) {
		t.Fatalf("visited %d regions (%v), want all %d — a failure must not cancel the siblings", len(visited), visited, len(regions))
	}
}

func TestAllRegionsSucceedingReturnsNoError(t *testing.T) {
	// Arrange
	c := clientOverRegions("us-east-1", "eu-west-1")

	// Act
	got, err := mapRegions(c, context.Background(), func(_ context.Context, region string, _ aws.Config) ([]string, error) {
		return []string{region}, nil
	})

	// Assert
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %v, want both regions", got)
	}
}
