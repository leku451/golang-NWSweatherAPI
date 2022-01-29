package transform

import (
	"bytes"
	"encoding/json"
	firebase "firebase.google.com/go"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

/************/
/* NWS/NOAA */
/************/

// The code below was ported from
// https://stackoverflow.com/questions/238260/how-to-calculate-the-bounding-box-for-a-given-lat-lng-location

// Semi-axes of WGS-84 geoidal reference
const (
	WGS84_a = 6378137.0 // Major semiaxis [m]
	WGS84_b = 6356752.3 // Minor semiaxis [m]
)

type MapPoint struct {
	Longitude float64
	Latitude  float64
}

type BoundingBox struct {
	MinPoint MapPoint
	MaxPoint MapPoint
}

// Deg2rad converts degrees to radians
func Deg2rad(degrees float64) float64 {
	return math.Pi * degrees / 180.0
}

// Rad2deg converts radians to degrees
func Rad2deg(radians float64) float64 {
	return 180.0 * radians / math.Pi
}

func WGS84EarthRadius(lat float64) float64 {
	An := WGS84_a * WGS84_a * math.Cos(lat)
	Bn := WGS84_b * WGS84_b * math.Sin(lat)
	Ad := WGS84_a * math.Cos(lat)
	Bd := WGS84_b * math.Sin(lat)
	return math.Sqrt((An*An + Bn*Bn) / (Ad*Ad + Bd*Bd))
}

// GetBoundingBox takes two arguments, MapPoint is a set of lat/lng,
// 'halfSideInKm' is the half length of the bounding box you want in kilometers.
func GetBoundingBox(point MapPoint, halfSideInKm float64) BoundingBox {
	// Bounding box surrounding the point at given coordinates,
	// assuming local approximation of Earth surface as a sphere
	// of radius given by WGS84
	lat := Deg2rad(point.Latitude)
	lon := Deg2rad(point.Longitude)
	halfSide := 1000 * halfSideInKm

	// Radius of Earth at given latitude
	radius := WGS84EarthRadius(lat)
	// Radius of the parallel at given latitude
	pradius := radius * math.Cos(lat)

	latMin := lat - halfSide/radius
	latMax := lat + halfSide/radius
	lonMin := lon - halfSide/pradius
	lonMax := lon + halfSide/pradius

	return BoundingBox{
		MinPoint: MapPoint{Latitude: Rad2deg(latMin), Longitude: Rad2deg(lonMin)},
		MaxPoint: MapPoint{Latitude: Rad2deg(latMax), Longitude: Rad2deg(lonMax)},
	}
}

// https://stackoverflow.com/questions/18390266/how-can-we-truncate-float64-type-to-a-particular-precision
func round(num float64) int {
	return int(num + math.Copysign(0.5, num))
}

func toFixed(num float64, precision int) float64 {
	output := math.Pow(10, float64(precision))
	return float64(round(num*output)) / output
}

func min(a, b int) int {
	if a <= b {
		return a
	}
	return b
}

type Geocode struct {
	UGC []string `json:"UGC"`
}

type Properties struct {
	ID            string   `json:"name"`
	AreaDesc      string   `json:"areaDesc"`
	Geocode       Geocode  `json:"geocode"`
	AffectedZones []string `json:"affectedZones"`
	Sent          string   `json:"sent"`
	Effective     string   `json:"effective"`
	Onset         string   `json:"onset"`
	Expires       string   `json:"expires"`
	Ends          string   `json:"ends"`
	Status        string   `json:"status"`
	MessageType   string   `json:"messageType"`
	Category      string   `json:"category"`
	Severity      string   `json:"severity"`
	Certainty     string   `json:"certainty"`
	Urgency       string   `json:"urgency"`
	Event         string   `json:"event"`
	Sender        string   `json:"sender"`
	SenderName    string   `json:"senderName"`
	Headline      string   `json:"headline"`
	Description   string   `json:"description"`
	Instruction   string   `json:"instruction"`
	Response      string   `json:"response"`
}

type WeatherAlert struct {
	ID         string     `json:"id"`
	Type       string     `json:"type"`
	Properties Properties `json:"properties"`
}

type WeatherObject struct {
	Features []WeatherAlert `json:"features"`
}

// GetWeatherAlerts will get active weather alerts from our categories we're
// interested in and write them to firebase
func GetWeatherAlerts(w http.ResponseWriter, r *http.Request) {
	var weatherAlertsURL = "https://api.weather.gov/alerts/active"

	resp, err := http.Get(weatherAlertsURL)
	if err != nil {
		fmt.Fprintf(w, "Error retrieving documents: %s", err)
		return
	}

	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	// w.Write(body)

	var m WeatherObject
	var savedWeathers []WeatherAlert
	if err := json.Unmarshal(body, &m); err != nil {
		panic(err)
	}
	for i := 0; i < len(m.Features); i++ {
		const severity = "Severe"
		sevCheck := strings.Contains(severity, m.Features[i].Properties.Severity)
		// https://www.weather.gov/media/documentation/docs/NWS_Geolocation.pdf
		eventList := [6]string{"Flash Flood Warning", "Severe Thunderstorm Warning", "Tornado Warning", "Snow Squall Warning", "Dust Storm Warning", "Extreme Wind Warning"}
		for _, event := range eventList {
			eventCheck := strings.Contains(event, m.Features[i].Properties.Event)
			if sevCheck {
				if eventCheck {
					// SaveWeatherAlerts(m.Features[i])
					// Push into an array instead and then add the array at the end.. derp
					savedWeathers = append(savedWeathers, m.Features[i])
				}
			}
		}
	}
	SaveWeatherAlerts(savedWeathers)
}

// GetWeatherAlertsAndRasters will get and sort through the latest
// weather alerts from NWS, we're looking for the bad stuff.
// It also attempts to download raster images based on a list of raster image IDs
// we retrieve from the API, and then it is going to save the reference to those IDs
// this mostly works up until the last 1-2 steps and starts to fall apart.
func GetWeatherAlertsAndRasters(w http.ResponseWriter, r *http.Request) {
	var weatherAlertsURL = "https://api.weather.gov/alerts/active"

	resp, err := http.Get(weatherAlertsURL)
	if err != nil {
		fmt.Fprintf(w, "Error retrieving documents: %s", err)
		return
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	// w.Write(body)

	var m WeatherObject
	if err := json.Unmarshal(body, &m); err != nil {
		panic(err)
	}
	for i := 0; i < len(m.Features); i++ {
		//w.Write([]byte(m[i].ID))
		const severity = "Severe"
		sevCheck := strings.Contains(severity, m.Features[i].Properties.Severity)

		// https://www.weather.gov/media/documentation/docs/NWS_Geolocation.pdf
		eventList := [5]string{"Severe Thunderstorm Warning", "Tornado Warning", "Snow Squall Warning", "Dust Storm Warning", "Extreme Wind Warning"}
		// const event = "Severe Thunderstorm Warning"
		for _, event := range eventList {

			eventCheck := strings.Contains(event, m.Features[i].Properties.Event)

			if sevCheck {
				if eventCheck {
					//SaveWeatherAlerts(m.Features[i])
					fmt.Fprintf(w, "Severity: %v Headline: %v\n", m.Features[i].Properties.Severity, m.Features[i].Properties.Event)
					if len(m.Features[i].Properties.AffectedZones) > 0 {
						for a := 0; a < len(m.Features[i].Properties.AffectedZones); a++ {
							// fmt.Fprintf(w, "AffectedZones: %v\n", m.Features[i].Properties.AffectedZones[a])
							aZone := m.Features[i].Properties.AffectedZones[a]

							fmt.Fprintf(w, "AffectedZones: %v\n", aZone)

							resp, err := http.Get(aZone)
							if err != nil {
								fmt.Fprintf(w, "Error retrieving documents: %s", err)
							}
							defer resp.Body.Close()
							countyBody, err := ioutil.ReadAll(resp.Body)

							type AlertData struct {
								ID       string `json:"id"`
								Type     string `json:"type"`
								Geometry struct {
									Type        string          `json:"type"`
									Coordinates [][][][]float64 `json:"coordinates"`
									Geometries  []struct {
										Type        string          `json:"type"`
										Coordinates [][][][]float64 `json:"coordinates"`
									} `json:"geometries"`
								} `json:"geometry"`
							}

							c := AlertData{}
							if err := json.Unmarshal(countyBody, &c); err != nil {
								panic(err)
							}

							var coordsSlice []string

							if len(c.Geometry.Geometries) > 0 {
								for geo := 0; geo < len(c.Geometry.Geometries); geo++ {
									if len(c.Geometry.Geometries[geo].Coordinates) > 0 {
										for z := 0; z < len(c.Geometry.Geometries[geo].Coordinates); z++ {
											lng := c.Geometry.Geometries[geo].Coordinates[0][0][z][0]
											lat := c.Geometry.Geometries[geo].Coordinates[0][0][z][1]
											sLat := fmt.Sprintf("%f", toFixed(lat, 4))
											sLng := fmt.Sprintf("%f", toFixed(lng, 4))
											coordsSlice = append(coordsSlice, sLat)
											coordsSlice = append(coordsSlice, sLng)
											// fmt.Fprintf(w, "%v,%v,", toFixed(lat, 4), toFixed(lng, 4))
											/*
												   boundinbox := GetBoundingBox(MapPoint{Latitude: lat, Longitude: lng},1)
												   fmt.Fprintf(w, "XMin: %v YMin: %v\n",
													   // X = longitude, Y = latitude
													   toFixed(boundinbox.MinPoint.Longitude, 4),
													   toFixed(boundinbox.MinPoint.Latitude, 4))
												   fmt.Fprintf(w, "XMax: %v YMax: %v\n",
													   toFixed(boundinbox.MaxPoint.Longitude, 4),
													   toFixed(boundinbox.MaxPoint.Latitude, 4))
											*/
										}
									}
								}
							} else {
								if len(c.Geometry.Coordinates) > 0 {
									for f := 0; f < len(c.Geometry.Coordinates[0][0]); f++ {
										lng := c.Geometry.Coordinates[0][0][f][0]
										lat := c.Geometry.Coordinates[0][0][f][1]
										// fmt.Fprintf(w, "%v,%v,", toFixed(lat, 4), toFixed(lng, 4))
										sLat := fmt.Sprintf("%f", toFixed(lat, 4))
										sLng := fmt.Sprintf("%f", toFixed(lng, 4))
										coordsSlice = append(coordsSlice, sLat)
										coordsSlice = append(coordsSlice, sLng)

										/*
											   boundinbox := GetBoundingBox(MapPoint{Latitude: lat, Longitude: lng},1)
											   fmt.Fprintf(w, "XMin: %v YMin: %v\n",
												   toFixed(boundinbox.MinPoint.Longitude, 4),
												   toFixed(boundinbox.MinPoint.Latitude, 4))
											   fmt.Fprintf(w, "XMax: %v YMax:%v\n",
												   toFixed(boundinbox.MaxPoint.Longitude, 4),
												   toFixed(boundinbox.MaxPoint.Latitude, 4))

										*/
									}
								}
							}

							// start processing coordinates into raster catalog requests
							result := strings.Join(coordsSlice, ",")

							// API can only handle so many requests in a URI
							limit := 1000
							// var imageIds []int

							if len(result) <= limit {
								imageQuery := "https://idpgis.ncep.noaa.gov/arcgis/rest/services/radar/radar_base_reflectivity_time/ImageServer/query" + "?geometry=" + result + "&f=json"
								// fmt.Fprintf(w, "ImageQuery: %v", imageQuery)
								imageIds := GetImageIDs(imageQuery)
								catalogItems := GetCatalogItems(imageIds)
								// fmt.Fprintf(w, "CatalogItemsCount: %v", len(catalogItems))
								for c := 0; c < len(catalogItems); c++ {
									for ring := 0; ring < len(catalogItems[c].Geometry.Rings[0][0]); ring++ {
										var rings = catalogItems[c].Geometry.Rings[0][ring]
										rasterImage := GetRasterImage(catalogItems[c].Attributes.Objectid, rings)
										fmt.Fprintf(w, "rings len: %v\n", rasterImage.Href)
										fmt.Fprintf(w, "Rasterimage: %v\n", rasterImage.Href)
										SaveRasterData(catalogItems[c].Attributes.Objectid, rings, rasterImage)
									}
								}
							} else {
								for cnt := 0; cnt < len(result); cnt += limit {
									batch := result[cnt:min(cnt+limit, len(result))]
									imageQuery := "https://idpgis.ncep.noaa.gov/arcgis/rest/services/radar/radar_base_reflectivity_time/ImageServer/query" + "?geometry=" + batch + "&f=json"
									// fmt.Fprintf(w, "ImageQuery2: %v", imageQuery)
									imageIds := GetImageIDs(imageQuery)
									catalogItems := GetCatalogItems(imageIds)
									for c := 0; c < len(catalogItems); c++ {
										for ring := 0; ring < len(catalogItems[c].Geometry.Rings[0][0]); ring++ {
											var rings = catalogItems[c].Geometry.Rings[0][ring]
											rasterImage := GetRasterImage(catalogItems[c].Attributes.Objectid, rings)
											fmt.Fprintf(w, "Rasterimage2: %v\n", rasterImage.Href)
											SaveRasterData(catalogItems[c].Attributes.Objectid, rings, rasterImage)
										}
									}
								}
							}
						}
					}
					/*
						 if eventCheck {
							 fmt.Fprintf(w, "Severity: %v Headline: %v\n",
												 m.Features[i].Properties.Severity,
												 m.Features[i].Properties.Event)
							 for x := 0; x < len(m.Features[i].Properties.Geocode.UGC); x++ {
								 fmt.Fprintf(w, "\tGeo: %v\n", m.Features[i].Properties.Geocode.UGC[x])
							 }
						 }
					*/
				}
			}
		}
	}
}

type CatalogItem struct {
	Attributes struct {
		Objectid           int         `json:"objectid"`
		Name               string      `json:"name"`
		Category           int         `json:"category"`
		IdpSource          interface{} `json:"idp_source"`
		IdpSubset          string      `json:"idp_subset"`
		IdpFiledate        int64       `json:"idp_filedate"`
		IdpIngestdate      int64       `json:"idp_ingestdate"`
		IdpCurrentForecast int         `json:"idp_current_forecast"`
		IdpTimeSeries      int         `json:"idp_time_series"`
		IdpIssueddate      int64       `json:"idp_issueddate"`
		IdpValidtime       int64       `json:"idp_validtime"`
		IdpValidendtime    int64       `json:"idp_validendtime"`
		ShapeLength        float64     `json:"shape_Length"`
		ShapeArea          float64     `json:"shape_Area"`
	} `json:"attributes"`
	Geometry struct {
		Rings            [][][]float64 `json:"rings"`
		SpatialReference struct {
			Wkid       int `json:"wkid"`
			LatestWkid int `json:"latestWkid"`
		} `json:"spatialReference"`
	} `json:"geometry"`
}

func GetCatalogItems(imageIds []int) []CatalogItem {
	var CatalogItems []CatalogItem
	for i := 0; i < len(imageIds); i++ {
		imageId := strconv.Itoa(imageIds[i])
		url := "https://idpgis.ncep.noaa.gov/arcgis/rest/services/radar/radar_base_reflectivity_time/ImageServer/" + imageId + "?f=pjson"
		resp, err := http.Get(url)
		if err != nil {
			log.Println(resp)
			panic(err)
		}
		defer resp.Body.Close()
		respBody, respBodyErr := ioutil.ReadAll(resp.Body)
		if respBodyErr != nil {
			log.Println(respBody)
			panic(respBodyErr)
		}

		catalogItem := CatalogItem{}
		if err := json.Unmarshal(respBody, &catalogItem); err != nil {
			panic(err)
		}
		CatalogItems = append(CatalogItems, catalogItem)
	}
	return CatalogItems
}

// SaveWeatherAlerts accepts one argument, a WeatherAlert object
// retrieved from NWS weather alerts REST endpoint.
func SaveWeatherAlerts(alerts []WeatherAlert) {
	// firebase setup - take the response body and write it to firestore
	// Use the application default credentials.
	conf := &firebase.Config{ProjectID: projectID, DatabaseURL: ""}

	// Use context.Background() because the app/client should persist across
	// invocations.
	ctx := context.Background()

	app, err := firebase.NewApp(ctx, conf)
	if err != nil {
		log.Fatalf("firebase.NewApp: %v", err)
	}
	client, err := app.Database(ctx)
	if err != nil {
		log.Fatalln("Error initializing the database client: ", err)
	}

	// Get a database reference to our radar raster image store
	ref := client.NewRef("alerts")
	alertRef := ref.Child("nws")
	alertRefErr := alertRef.Set(ctx, alerts)
	if alertRefErr != nil {
		log.Fatalln("Error saving weather alerts in google realtime db: ", err)
	}
}

// SaveRasterData takes two arguments and will save save our raster data to firebase
// realtime database
func SaveRasterData(objectId int, area []float64, rasterImage RasterImage) {
	// firebase setup - take the response body and write it to firestore
	// Use the application default credentials.
	conf := &firebase.Config{ProjectID: projectID, DatabaseURL: ""}

	// Use context.Background() because the app/client should persist across
	// invocations.
	ctx := context.Background()

	app, err := firebase.NewApp(ctx, conf)
	if err != nil {
		log.Fatalf("firebase.NewApp: %v", err)
	}
	client, err := app.Database(ctx)
	if err != nil {
		log.Fatalln("Error initializing the database client: ", err)
	}

	// Get a database reference to our radar raster image store
	ref := client.NewRef("radar")
	radarRef := ref.Child(strconv.Itoa(objectId))
	radarRefErr := radarRef.Update(ctx, map[string]interface{}{"ObjectId": objectId, "RasterImage": rasterImage, "Area": area})
	if radarRefErr != nil {
		log.Fatalln("Error saving Raster Data in google realtime db: ", err)
	}
}

type RasterImage struct {
	Href   string `json:"href"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Extent struct {
		Xmin             float64 `json:"xmin"`
		Ymin             float64 `json:"ymin"`
		Xmax             float64 `json:"xmax"`
		Ymax             float64 `json:"ymax"`
		SpatialReference struct {
			Wkid       int `json:"wkid"`
			LatestWkid int `json:"latestWkid"`
		} `json:"spatialReference"`
	} `json:"extent"`
	Scale int `json:"scale"`
}

func GetRasterImage(objectId int, bbox []float64) RasterImage {
	sObjectId := strconv.Itoa(objectId)
	// sBoundingBox := strings.Join(bbox, ",")

	var BBox []string
	for b := 0; b < len(bbox); b++ {
		log.Println(bbox[b])
		sB := fmt.Sprintf("%f", bbox[b])
		BBox = append(BBox, sB)
	}

	sBBox := strings.Join(BBox, ",")

	url := "https://idpgis.ncep.noaa.gov/arcgis/rest/services/radar/radar_base_reflectivity_time/ImageServer/" + sObjectId + "/image?bbox=" + sBBox + "&f=pjson"
	log.Printf("URL: %v\n", url)
	resp, err := http.Get(url)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	respBody, respErr := ioutil.ReadAll(resp.Body)

	if respErr != nil {
		panic(respErr)
	}

	rasterImage := RasterImage{}
	if err := json.Unmarshal(respBody, &rasterImage); err != nil {
		panic(err)
	}

	return rasterImage
}

func GetImageIDs(imageQuery string) []int {
	imageResp, imageErr := http.Get(imageQuery)
	if imageErr != nil {
		panic(imageErr)
	}
	defer imageResp.Body.Close()
	imageRespBody, imageRespErr := ioutil.ReadAll(imageResp.Body)
	if imageRespErr != nil {
		panic(imageRespErr)
	}
	type ImageQuery struct {
		ObjectIDFieldName string `json:"objectIdFieldName"`
		Fields            []struct {
			Name   string      `json:"name"`
			Type   string      `json:"type"`
			Alias  string      `json:"alias"`
			Domain interface{} `json:"domain"`
		} `json:"fields"`
		GeometryType     string `json:"geometryType"`
		SpatialReference struct {
			Wkid       int `json:"wkid"`
			LatestWkid int `json:"latestWkid"`
		} `json:"spatialReference"`
		Features []struct {
			Attributes struct {
				Objectid    int     `json:"objectid"`
				ShapeLength float64 `json:"shape_Length"`
				ShapeArea   float64 `json:"shape_Area"`
			} `json:"attributes"`
			Geometry struct {
				Rings            [][][]float64 `json:"rings"`
				SpatialReference struct {
					Wkid       int `json:"wkid"`
					LatestWkid int `json:"latestWkid"`
				} `json:"spatialReference"`
			} `json:"geometry"`
		} `json:"features"`
	}

	imgQuery := ImageQuery{}
	if err := json.Unmarshal(imageRespBody, &imgQuery); err != nil {
		panic(err)
	}

	var objectIdArray []int
	// var weatherstationsArray []WeatherStation
	for feature := 0; feature < len(imgQuery.Features); feature++ {
		objectId := imgQuery.Features[feature].Attributes.Objectid
		objectIdArray = append(objectIdArray, objectId)
	}

	return objectIdArray
}

// UpdateWeather is responsible for keeping the weather stations and
// severe weather alert tracking system up to date. Should be run out of
// google cron system every 5 minutes
func UpdateWeather(w http.ResponseWriter, r *http.Request) {
	var ListStationsURL = ""
	resp, err := http.Get(ListStationsURL)
	if err != nil {
		fmt.Fprintf(w, "Error retrieving documents: %s", err)
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)

	type WeatherStation struct {
		ID        string
		CreatedAt string
	}

	var m []WeatherStation
	if err := json.Unmarshal(body, &m); err != nil {
		panic(err)
	}
	for i := 0; i < len(m); i++ {
		// call the UpdateStation API endpoint for each station ID
		postBody, _ := json.Marshal(map[string]string{
			"ID": m[i].ID,
		})
		responseBody := bytes.NewBuffer(postBody)
		resp, err = http.Post("https://localhost/GetWeather",
			"application/json", responseBody)
		if err != nil {
			log.Fatalf("An error occured %v", err)
		}
		defer resp.Body.Close()
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			log.Fatalln(err)
		}
		sb := string(body)
		fmt.Fprintln(w, sb)
	}
}

// GetWeather takes the ID of a weather station and returns the data from the
// NOAA API
func GetWeather(w http.ResponseWriter, r *http.Request) {
	// Inputs
	var d struct {
		ID string `json:"id"`
	}

	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		fmt.Fprint(w, "Error, cannot decode body of request")
		return
	}

	if d.ID == "" {
		fmt.Fprint(w, "Must supply weather station ID!")
		return
	}

	var weatherUrl = "https://api.tidesandcurrents.noaa.gov/mdapi/prod/webapi/stations/" + d.ID + ".json"
	resp, err := http.Get(weatherUrl)
	if err != nil {
		fmt.Fprintf(w, "Error retrieving documents: %s", err)
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)

	m := map[string]interface{}{}
	if err := json.Unmarshal(body, &m); err != nil {
		panic(err)
	}
	fmt.Printf("%q", m)

	// firebase setup - take the response body and write it to firestore
	// Use the application default credentials.
	conf := &firebase.Config{ProjectID: projectID}

	// Use context.Background() because the app/client should persist across
	// invocations.
	ctx := context.Background()

	app, err := firebase.NewApp(ctx, conf)
	if err != nil {
		log.Fatalf("firebase.NewApp: %v", err)
	}

	client, err = app.Firestore(ctx)
	if err != nil {
		log.Fatalf("app.Firestore: %v", err)
	}

	defer func(client *firestore.Client) {
		err := client.Close()
		if err != nil {
			log.Fatalf("client.Close: #{err}")
		}
	}(client)

	// id, err := uuid.NewUUID()

	myRes, addErr := client.Collection("weather").Doc(d.ID).Set(ctx, m)
	if addErr != nil {
		// Handle any errors in an appropriate way, such as returning them.
		log.Printf("An error has occurred: %v Response: %v", addErr, myRes)
		addErrMsgJSON, addErr := json.Marshal(addErr)
		fmt.Fprint(w, addErr)
		_, err := w.Write(addErrMsgJSON)
		if err != nil {
			return
		}
	}

	resJSON, resErr := json.Marshal(d.ID)
	if resErr != nil {
		fmt.Fprintf(w, "Error marshaling JSON: %s", resErr)
		return
	}

	// Return a response to the client, including the ID of the newly created document
	_, err = w.Write(resJSON)
	if err != nil {
		return
	}

}

/*********************************/
/* WEATHER STATIONS - STORMSURGE */
/*********************************/

// ListStations lists weather station IDs we are tracking in firebase
func ListStations(w http.ResponseWriter, r *http.Request) {
	// Use the application default credentials.
	conf := &firebase.Config{ProjectID: projectID}

	// Use context.Background() because the app/client should persist across
	// invocations.
	ctx := context.Background()

	app, err := firebase.NewApp(ctx, conf)
	if err != nil {
		log.Fatalf("firebase.NewApp: %v", err)
	}

	client, err = app.Firestore(ctx)
	if err != nil {
		log.Fatalf("app.Firestore: %v", err)
	}

	defer func(client *firestore.Client) {
		err := client.Close()
		if err != nil {
			log.Fatalf("client.Close: #{err}")
		}
	}(client)

	type WeatherStation struct {
		ID        string    `firestore:"stationId"`
		CreatedAt time.Time `firestore:"createdAt"`
	}

	weatherstations := client.Collection("weatherstations")
	docs, err := weatherstations.Documents(ctx).GetAll()
	if err != nil {
		fmt.Fprint(w, "Error retrieving documents")
		return
	}

	var weatherstationsArray []WeatherStation

	for _, doc := range docs {
		var weatherstationData WeatherStation
		if err := doc.DataTo(&weatherstationData); err != nil {
			fmt.Fprintf(w, "Error retrieving documents: %s", err)
			return
		}
		weatherstationsArray = append(weatherstationsArray, weatherstationData)
	}

	// Convert our array to JSON and spit it out
	js, err := json.Marshal(weatherstationsArray)
	if err != nil {
		fmt.Fprintf(w, "Error marshaling JSON: %s", err)
		return
	}
	w.Write(js)
}

type Boats struct {
	MMSI  string
	Group string
	Type  string
}
