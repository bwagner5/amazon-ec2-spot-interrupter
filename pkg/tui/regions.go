package tui

import "strings"

var regionLocationByCode = map[string]string{
	"af-south-1":     "Cape Town",
	"ap-east-1":      "Hong Kong",
	"ap-east-2":      "Taipei",
	"ap-northeast-1": "Tokyo",
	"ap-northeast-2": "Seoul",
	"ap-northeast-3": "Osaka",
	"ap-south-1":     "Mumbai",
	"ap-south-2":     "Hyderabad",
	"ap-southeast-1": "Singapore",
	"ap-southeast-2": "Sydney",
	"ap-southeast-3": "Jakarta",
	"ap-southeast-4": "Melbourne",
	"ap-southeast-5": "Malaysia",
	"ap-southeast-6": "Auckland",
	"ap-southeast-7": "Thailand",
	"ca-central-1":   "Canada Central",
	"ca-west-1":      "Calgary",
	"cn-north-1":     "Beijing",
	"cn-northwest-1": "Ningxia",
	"eu-central-1":   "Frankfurt",
	"eu-central-2":   "Zurich",
	"eu-north-1":     "Stockholm",
	"eu-south-1":     "Milan",
	"eu-south-2":     "Spain",
	"eu-west-1":      "Ireland",
	"eu-west-2":      "London",
	"eu-west-3":      "Paris",
	"il-central-1":   "Tel Aviv",
	"me-central-1":   "UAE",
	"me-south-1":     "Bahrain",
	"mx-central-1":   "Queretaro",
	"sa-east-1":      "Sao Paulo",
	"us-east-1":      "N. Virginia",
	"us-east-2":      "Ohio",
	"us-west-1":      "N. California",
	"us-west-2":      "Oregon",
	"us-gov-east-1":  "GovCloud US-East",
	"us-gov-west-1":  "GovCloud US-West",
}

func regionLocation(region string) string {
	return regionLocationByCode[region]
}

func regionGroupLabel(region string) string {
	switch {
	case strings.HasPrefix(region, "us-gov-"):
		return "US GovCloud"
	case strings.HasPrefix(region, "us-"):
		return "US"
	case strings.HasPrefix(region, "ca-"):
		return "Canada"
	case strings.HasPrefix(region, "mx-"):
		return "Mexico"
	case strings.HasPrefix(region, "eu-"):
		return "Europe"
	case strings.HasPrefix(region, "il-"):
		return "Israel"
	case strings.HasPrefix(region, "me-"):
		return "Middle East"
	case strings.HasPrefix(region, "af-"):
		return "Africa"
	case strings.HasPrefix(region, "ap-"):
		return "Asia Pacific"
	case strings.HasPrefix(region, "sa-"):
		return "South America"
	case strings.HasPrefix(region, "cn-"):
		return "China"
	default:
		return "Other"
	}
}

func regionGroupRank(region string) int {
	switch regionGroupLabel(region) {
	case "US":
		return 0
	case "US GovCloud":
		return 1
	case "Canada":
		return 2
	case "Mexico":
		return 3
	case "Europe":
		return 4
	case "Israel":
		return 5
	case "Middle East":
		return 6
	case "Africa":
		return 7
	case "Asia Pacific":
		return 8
	case "South America":
		return 9
	case "China":
		return 10
	default:
		return 11
	}
}
