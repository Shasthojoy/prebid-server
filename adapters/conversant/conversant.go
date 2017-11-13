package conversant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"

	"github.com/mxmCherry/openrtb"
	"github.com/prebid/prebid-server/adapters"
	"github.com/prebid/prebid-server/pbs"
	"golang.org/x/net/context/ctxhttp"
)

type ConversantAdapter struct {
	http         *adapters.HTTPAdapter
	URI          string
	usersyncInfo *pbs.UsersyncInfo
}

type FlexBool bool

// Allows boolean values to use either true/false or 1/0

func (flag *FlexBool) UnmarshalJSON(data []byte) (err error) {
	bval, nval := false, int8(0)
	// Check if true/false is used
	if err = json.Unmarshal(data, &bval); err == nil {
		*flag = FlexBool(bval)
		return
	}
	// Check if a number is used
	if err = json.Unmarshal(data, &nval); err == nil {
		if nval != 0 {
			*flag = true
		} else {
			*flag = false
		}
		return
	}
	// Anything else is an error
	return
}

// Return 1/0 for boolean

func (flag *FlexBool) ToInt8() int8 {
	if *flag == true {
		return 1
	}
	return 0
}

// Name - export adapter name
func (a *ConversantAdapter) Name() string {
	return "Conversant"
}

// Corresponds to the bidder name in cookies and requests
func (a *ConversantAdapter) FamilyName() string {
	return "conversant"
}

func (a *ConversantAdapter) GetUsersyncInfo() *pbs.UsersyncInfo {
	return a.usersyncInfo
}

func (a *ConversantAdapter) SkipNoCookies() bool {
	return false
}

type conversantParams struct {
	SiteID      string    `json:"site_id"`
	Secure      *FlexBool `json:"secure"`
	TagID       string    `json:"tag_id"`
	Position    *int8     `json:"position"`
	BidFloor    float64   `json:"bidfloor"`
	Mobile      *FlexBool `json:"mobile"`
	MIMEs       []string  `json:"mimes"`
	API         []int8    `json:"api"`
	Protocols   []int8    `json:"protocols"`
	MaxDuration *int64    `json:"maxduration"`
}

func (a *ConversantAdapter) Call(ctx context.Context, req *pbs.PBSRequest, bidder *pbs.PBSBidder) (pbs.PBSBidSlice, error) {
	mediaTypes := []pbs.MediaType{pbs.MEDIA_TYPE_BANNER, pbs.MEDIA_TYPE_VIDEO}
	cnvrReq, err := adapters.MakeOpenRTBGeneric(req, bidder, a.FamilyName(), mediaTypes, true)

	if err != nil {
		return nil, err
	}

	// Create a map of impression objects for both request creation
	// and response parsing.

	impMap := make(map[string]*openrtb.Imp)
	for idx, _ := range cnvrReq.Imp {
		impMap[cnvrReq.Imp[idx].ID] = &cnvrReq.Imp[idx]
	}

	// Fill in additional info from custom params

	for _, unit := range bidder.AdUnits {
		var params conversantParams

		imp := impMap[unit.Code]
		if imp == nil {
			// Skip ad units that do not have corresponding impressions.
			continue
		}

		err := json.Unmarshal(unit.Params, &params)
		if err != nil {
			return nil, err
		}

		// Fill in additional Site info

		if params.SiteID != "" {
			cnvrReq.Site.ID = params.SiteID
		}

		if params.Mobile != nil {
			cnvrReq.Site.Mobile = params.Mobile.ToInt8()
		}

		// Fill in additional impression info

		imp.DisplayManager = "prebid-s2s"
		imp.BidFloor = params.BidFloor
		imp.TagID = params.TagID

		var position *openrtb.AdPosition
		if params.Position != nil {
			position = openrtb.AdPosition(*params.Position).Ptr()
		}

		if imp.Banner != nil {
			imp.Banner.Pos = position
		} else if imp.Video != nil {
			imp.Video.Pos = position

			if len(params.API) > 0 {
				imp.Video.API = make([]openrtb.APIFramework, 0, len(params.API))
				for _, api := range params.API {
					imp.Video.API = append(imp.Video.API, openrtb.APIFramework(api))
				}
			}

			// Include protocols, mimes, and max duration if specified
			// These properties can also be specified in ad unit's video object,
			// but are overriden if the custom params object also contains them.

			if len(params.Protocols) > 0 {
				imp.Video.Protocols = make([]openrtb.Protocol, 0, len(params.Protocols))
				for _, protocol := range params.Protocols {
					imp.Video.Protocols = append(imp.Video.Protocols, openrtb.Protocol(protocol))
				}
			}

			if len(params.MIMEs) > 0 {
				imp.Video.MIMEs = make([]string, len(params.MIMEs))
				copy(imp.Video.MIMEs, params.MIMEs)
			}

			if params.MaxDuration != nil {
				imp.Video.MaxDuration = *params.MaxDuration
			}
		}

		// Take care not to override the global secure flag

		if (imp.Secure == nil || *imp.Secure == 0) && params.Secure != nil {
			imp.Secure = openrtb.Int8Ptr(params.Secure.ToInt8())
		}
	}

	// Do a quick check on required parameters

	if cnvrReq.Site.ID == "" {
		return nil, fmt.Errorf("Missing site id")
	}

	// Start capturing debug info

	debug := &pbs.BidderDebug{
		RequestURI: a.URI,
	}

	if cnvrReq.Device == nil {
		cnvrReq.Device = &openrtb.Device{}
	}

	// Convert request to json to be sent over http

	j, _ := json.Marshal(cnvrReq)

	if req.IsDebug {
		debug.RequestBody = string(j)
		bidder.Debug = append(bidder.Debug, debug)
	}

	httpReq, err := http.NewRequest("POST", a.URI, bytes.NewBuffer(j))
	httpReq.Header.Add("Content-Type", "application/json")
	httpReq.Header.Add("Accept", "application/json")

	resp, err := ctxhttp.Do(ctx, a.http.Client, httpReq)
	if err != nil {
		return nil, err
	}

	if req.IsDebug {
		debug.StatusCode = resp.StatusCode
	}

	if resp.StatusCode == 204 {
		return nil, nil
	}

	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)

	if err != nil {
		return nil, err
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP status: %d, body: %s", resp.StatusCode, string(body))
	}

	if req.IsDebug {
		debug.ResponseBody = string(body)
	}

	var bidResp openrtb.BidResponse

	err = json.Unmarshal(body, &bidResp)
	if err != nil {
		return nil, err
	}

	bids := make(pbs.PBSBidSlice, 0)

	for _, seatbid := range bidResp.SeatBid {
		for _, bid := range seatbid.Bid {
			if bid.Price <= 0 {
				continue
			}

			imp := impMap[bid.ImpID]
			if imp == nil {
				// All returned bids should have a matching impression
				return nil, fmt.Errorf("Unknown impression id '%s'", bid.ImpID)
			}

			bidID := bidder.LookupBidID(bid.ImpID)
			if bidID == "" {
				return nil, fmt.Errorf("Unknown ad unit code '%s'", bid.ImpID)
			}

			pbsBid := pbs.PBSBid{
				BidID:       bidID,
				AdUnitCode:  bid.ImpID,
				Price:       bid.Price,
				Creative_id: bid.CrID,
				BidderCode:  bidder.BidderCode,
			}

			if imp.Video != nil {
				pbsBid.CreativeMediaType = "video"
				pbsBid.NURL = bid.AdM // Assign to NURL so it'll be interpreted as a vastUrl
				pbsBid.Width = imp.Video.W
				pbsBid.Height = imp.Video.H
			} else {
				pbsBid.CreativeMediaType = "banner"
				pbsBid.NURL = bid.NURL
				pbsBid.Adm = bid.AdM
				pbsBid.Width = bid.W
				pbsBid.Height = bid.H
			}

			bids = append(bids, &pbsBid)
		}
	}

	if len(bids) == 0 {
		return nil, nil
	}

	return bids, nil
}

func NewConversantAdapter(config *adapters.HTTPAdapterConfig, uri string, usersyncURL string, externalURL string) *ConversantAdapter {
	a := adapters.NewHTTPAdapter(config)
	redirect_uri := fmt.Sprintf("%s/setuid?bidder=conversant&uid=", externalURL)

	info := &pbs.UsersyncInfo{
		URL:         fmt.Sprintf("%s%s", usersyncURL, url.QueryEscape(redirect_uri)),
		Type:        "redirect",
		SupportCORS: false,
	}

	return &ConversantAdapter{
		http:         a,
		URI:          uri,
		usersyncInfo: info,
	}
}
