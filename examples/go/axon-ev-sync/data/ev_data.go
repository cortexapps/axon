package data

import (
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"

	"go.uber.org/zap"
)

const EvDataUri = "https://data.wa.gov/resource/f6w7-q2d2.csv"

type Loader struct {
	appToken string
	logger   *zap.Logger
}

func NewLoader(logger *zap.Logger) *Loader {
	appToken := os.Getenv("APP_TOKEN")
	return &Loader{appToken: appToken, logger: logger}
}

func (l *Loader) Load() (<-chan *EVData, error) {

	channel := make(chan *EVData, 100000)

	pageSize := 1000
	page := 0
	client := &http.Client{}

	go func() {
		for {

			uri := fmt.Sprintf("%s?$limit=%d&$offset=%d", EvDataUri, pageSize, page*pageSize)
			req, err := http.NewRequest("GET", uri, nil)
			if err != nil {
				l.logger.Error("error creating request: %v", zap.Error(err))
				return
			}
			page++

			// Set headers
			if l.appToken != "" {
				req.Header.Set("X-App-Token", l.appToken)
			}
			req.Header.Set("Accept", "text/csv")

			resp, err := client.Do(req)

			if err != nil {
				l.logger.Sugar().Errorf("error fetching data: %v", err)
				return
			}

			if resp.StatusCode != http.StatusOK {
				l.logger.Sugar().Errorf("error fetching data: status code %d", resp.StatusCode)
				return
			}

			err = l.load(resp.Body, channel)
			if err == io.EOF {
				return
			}
			if err != nil {
				l.logger.Sugar().Errorf("Error loading data: %v", err)
				return
			}
		}
	}()
	return channel, nil
}

func (l *Loader) load(data io.ReadCloser, output chan *EVData) error {

	reader := csv.NewReader(data)
	reader.FieldsPerRecord = -1 // Allow variable number of fields per record

	defer data.Close()

	_, err := reader.Read() // Skip header
	if err != nil {
		data.Close()
		return err
	}

	empty := true

	for {
		record, readerErr := reader.Read()

		if readerErr == io.EOF {
			if empty {
				close(output)
				return io.EOF
			}
			break
		}
		empty = false

		if readerErr != nil {
			err = readerErr
		}

		modelYear, _ := strconv.Atoi(record[5])
		electricRange, _ := strconv.Atoi(record[10])
		baseMSRP, _ := strconv.Atoi(record[11])
		legislativeDistrict, _ := strconv.Atoi(record[12])
		dolVehicleID, _ := strconv.Atoi(record[13])

		evData := EVData{
			VIN:                 record[0],
			County:              record[1],
			City:                record[2],
			State:               record[3],
			PostalCode:          record[4],
			ModelYear:           modelYear,
			Make:                record[6],
			Model:               record[7],
			ElectricVehicleType: record[8],
			CAFVEligibility:     record[9],
			ElectricRange:       electricRange,
			BaseMSRP:            baseMSRP,
			LegislativeDistrict: legislativeDistrict,
			DOLVehicleID:        dolVehicleID,
			VehicleLocation:     record[14],
			ElectricUtility:     record[15],
			CensusTract:         record[16],
		}

		output <- &evData
	}

	return err
}

// Exapmle CSV row
//
// VIN (1-10),County,City,State,Postal Code,Model Year,Make,Model,Electric Vehicle Type,Clean Alternative Fuel Vehicle (CAFV) Eligibility,Electric Range,Base MSRP,Legislative District,DOL Vehicle ID,Vehicle Location,Electric Utility,2020 Census Tract
// 1N4AZ0CP8D,King,Shoreline,WA,98177,2013,NISSAN,LEAF,Battery Electric Vehicle (BEV),Clean Alternative Fuel Vehicle Eligible,75,0,32,125450447,POINT (-122.36498 47.72238),CITY OF SEATTLE - (WA)|CITY OF TACOMA - (WA),53033020100
// 5YJSA1E45K,King,Seattle,WA,98112,2019,TESLA,MODEL S,Battery Electric Vehicle (BEV),Clean Alternative Fuel Vehicle Eligible,270,0,43,101662900,POINT (-122.30207 47.64085),CITY OF SEATTLE - (WA)|CITY OF TACOMA - (WA),53033006300
// WVGUNPE28M,Kitsap,Olalla,WA,98359,2021,VOLKSWAGEN,ID.4,Battery Electric Vehicle (BEV),Eligibility unknown as battery range has not been researched,0,0,26,272118717,POINT (-122.54729 47.42602),PUGET SOUND ENERGY INC,53035092803
// JTDKARFP6H,Thurston,Olympia,WA,98501,2017,TOYOTA,PRIUS PRIME,Plug-in Hybrid Electric Vehicle (PHEV),Not eligible due to low battery range,25,0,22,349372929,POINT (-122.89166 47.03956),PUGET SOUND ENERGY INC,53067010400

type EVData struct {
	VIN                 string
	DOLVehicleID        int
	County              string
	City                string
	State               string
	PostalCode          string
	ModelYear           int
	Make                string
	Model               string
	ElectricVehicleType string
	CAFVEligibility     string
	ElectricRange       int
	BaseMSRP            int
	LegislativeDistrict int
	VehicleLocation     string
	ElectricUtility     string
	CensusTract         string
}

func (evd *EVData) EvType() string {
	switch evd.ElectricVehicleType {
	case "Battery Electric Vehicle (BEV)":
		return "BEV"
	case "Plug-in Hybrid Electric Vehicle (PHEV)":
		return "PHEV"
	default:
		return "Unknown"
	}
}
