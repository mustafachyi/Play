package youtube

type clientProfile struct {
	numericID string
	context   clientContext
}

type clientContext struct {
	ClientName       string `json:"clientName"`
	ClientVersion    string `json:"clientVersion"`
	UserAgent        string `json:"userAgent,omitempty"`
	HL               string `json:"hl"`
	GL               string `json:"gl,omitempty"`
	TimeZone         string `json:"timeZone"`
	UTCOffsetMinutes int    `json:"utcOffsetMinutes"`
	DeviceMake       string `json:"deviceMake,omitempty"`
	DeviceModel      string `json:"deviceModel,omitempty"`
	AndroidSDK       int    `json:"androidSdkVersion,omitempty"`
	OSName           string `json:"osName,omitempty"`
	OSVersion        string `json:"osVersion,omitempty"`
	VisitorData      string `json:"visitorData,omitempty"`
}

var playerProfiles = []clientProfile{
	{
		numericID: "101",
		context: clientContext{
			ClientName:       "VISIONOS",
			ClientVersion:    "1.02",
			DeviceMake:       "Apple",
			DeviceModel:      "RealityDevice17,1",
			OSName:           "visionOS",
			OSVersion:        "26.5.23O471",
			UserAgent:        "Mozilla/5.0 (Macintosh; Intel Mac OS X 15_7_3) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Safari/605.1.15",
			HL:               "en",
			TimeZone:         "UTC",
			UTCOffsetMinutes: 0,
		},
	},
}

var browseProfile = clientProfile{
	numericID: "1",
	context: clientContext{
		ClientName:       "WEB",
		ClientVersion:    "2.20260708.00.00",
		HL:               "en",
		GL:               "US",
		TimeZone:         "UTC",
		UTCOffsetMinutes: 0,
	},
}
