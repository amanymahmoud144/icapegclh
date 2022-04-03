package api

import (
	"bytes"
	"errors"
	"fmt"
	zLog "github.com/rs/zerolog/log"
	"icapeg/config"
	"icapeg/icap"
	"icapeg/logger"
	"icapeg/readValues"
	"icapeg/service"
	"icapeg/utils"
	"io"
	"io/ioutil"
	"net/http"
	"strconv"
	"time"
)

// ICAPRequest struct is used to encapsulate important information of the ICAP request like method name, etc
type ICAPRequest struct {
	w                      icap.ResponseWriter
	req                    *icap.Request
	h                      http.Header
	Is204Allowed           bool
	isShadowServiceEnabled bool
	appCfg                 *config.AppConfig
	logger                 *logger.ZLogger
	elapsed                time.Duration
	serviceName            string
	methodName             string
	vendor                 string
}

//NewICAPRequest is a func to create a new instance from struct IcapRequest yo handle upcoming ICAP requests
func NewICAPRequest(w icap.ResponseWriter, req *icap.Request, logger *logger.ZLogger) *ICAPRequest {
	ICAPRequest := &ICAPRequest{
		w:       w,
		req:     req,
		h:       w.Header(),
		logger:  logger,
		elapsed: time.Since(logger.LogStartTime),
	}
	return ICAPRequest
}

//RequestInitialization is a fun to retrieve the important information from the ICAP request
//and initialize the ICAP response
func (i *ICAPRequest) RequestInitialization() error {

	//adding headers to the log
	i.addHeadersToLogs()

	// checking if the service doesn't exist in toml file
	// if it does not exist, the response will be 404 ICAP Service Not Found
	i.serviceName = i.req.URL.Path[1:len(i.req.URL.Path)]
	if !i.isServiceExists() {
		i.w.WriteHeader(http.StatusNotFound, nil, false)
		err := errors.New("service doesn't exist")
		return err
	}

	// checking if request method is allowed or not
	i.methodName = i.req.Method
	i.methodName = i.getMethodName()
	if !i.isMethodAllowed() {
		i.w.WriteHeader(http.StatusMethodNotAllowed, nil, false)
		err := errors.New("method is not allowed")
		return err
	}
	i.methodName = i.req.Method

	//getting vendor name which depends on the name of the service
	i.vendor = i.getVendorName()

	//adding important headers to options ICAP response
	i.addingISTAGServiceHeaders()

	i.Is204Allowed = i.is204Allowed()

	i.isShadowServiceEnabled = readValues.ReadValuesBool(i.serviceName + ".shadow_service")

	i.appCfg = config.App()

	//checking if the shadow service is enabled or not to apply shadow service mode
	if i.isShadowServiceEnabled && i.methodName != "OPTIONS" {
		i.shadowService()
		go i.RequestProcessing()
		return errors.New("shadow service")
	}

	return nil
}

//RequestProcessing is a func to process the ICAP request upon the service and method required
func (i *ICAPRequest) RequestProcessing() {

	// check the method name
	switch i.methodName {
	case utils.ICAPModeOptions:
		i.optionsMode()
		break

	//for reqmod and respmod
	default:
		if i.methodName == "REQMOD" {
			defer i.req.Request.Body.Close()
			if i.req.Request != nil {
				if i.req.Request.Method == "CONNECT" {
					i.w.WriteHeader(utils.NoModificationStatusCodeStr, nil, false)
					return
				}
			}
		} else {
			defer i.req.Response.Body.Close()
		}
		if i.req.Request == nil {
			i.req.Request = &http.Request{}
		}
		//initialize the service by creating instance from the required service
		requiredService := service.GetService(i.vendor, i.serviceName, i.methodName,
			&utils.HttpMsg{Request: i.req.Request, Response: i.req.Response}, i.elapsed, i.logger)

		//calling Processing func to process the http message which encapsulated inside the ICAP request
		IcapStatusCode, httpMsg, serviceHeaders := requiredService.Processing()

		// adding the headers which the service wants to add them in the ICAP response
		if serviceHeaders != nil {
			for key, value := range serviceHeaders {
				i.w.Header().Set(key, value)
			}
		}

		//checking if shadw service mode is enabled to add logs instead of returning another
		//ICAP response beside the one who was sebt to the client in line 88
		if i.isShadowServiceEnabled {
			//add logs here
			return
		}

		//check the ICAP status code which returned from the service to decide
		//how should be the ICAP response
		switch IcapStatusCode {
		case utils.InternalServerErrStatusCodeStr:
			i.w.WriteHeader(IcapStatusCode, nil, false)
			break
		case utils.NoModificationStatusCodeStr:
			if i.Is204Allowed {
				i.w.WriteHeader(utils.NoModificationStatusCodeStr, nil, false)
			} else {
				i.w.WriteHeader(utils.OkStatusCodeStr, httpMsg, true)
			}
		case utils.OkStatusCodeStr:
			i.w.WriteHeader(utils.OkStatusCodeStr, httpMsg, true)
		}

	}

}

//adding headers to the log
func (i *ICAPRequest) addHeadersToLogs() {
	for key, element := range i.req.Header {
		res := key + " : "
		for i := 0; i < len(element); i++ {
			res += element[i]
			if i != len(element)-1 {
				res += ", "
			}
		}
		zLog.Debug().Dur("duration", i.elapsed).Str("value", "ICAP request header").
			Msgf(res)
	}
}

//isServiceExists is a func to make sure that service which required in ICAP
//request is existing in the config file
func (i *ICAPRequest) isServiceExists() bool {
	services := readValues.ReadValuesSlice("app.services")
	for r := 0; r < len(services); r++ {
		if i.serviceName == services[r] {
			return true
		}
	}
	return false

}

//getMethodName is a func to get the name of the method of the ICAP request
func (i *ICAPRequest) getMethodName() string {
	if i.methodName == "REQMOD" {
		i.methodName = "req_mode"
	} else if i.methodName == "RESPMOD" {
		i.methodName = "resp_mode"
	}
	return i.methodName
}

//isMethodAllowed is a func to check if the method in the ICAP request is allowed in config file or not
func (i *ICAPRequest) isMethodAllowed() bool {
	if i.methodName != "OPTIONS" {
		isMethodEnabled := readValues.ReadValuesBool(i.serviceName + "." + i.methodName)
		if !isMethodEnabled {
			zLog.Debug().Dur("duration", i.elapsed).Str("value", i.methodName+" is not enabled").
				Msgf("this_method_is_not_enabled_in_GO_ICAP_configuration")
			return false
		}
	}
	return true
}

//getVendorName is a func to get the vendor of the service which in the ICAP request
func (i *ICAPRequest) getVendorName() string {
	vendor := i.serviceName + ".vendor"
	vendor = readValues.ReadValuesString(vendor)
	return vendor
}

//addingISTAGServiceHeaders is a func to add the important header to ICAP response
func (i *ICAPRequest) addingISTAGServiceHeaders() {
	i.h.Set("ISTag", readValues.ReadValuesString(i.serviceName+".service_tag"))
	i.h.Set("Service", readValues.ReadValuesString(i.serviceName+".service_caption"))
	zLog.Info().Dur("duration", i.elapsed).Str("value", fmt.Sprintf("with method:%s url:%s", i.methodName, i.req.RawURL)).
		Msgf("request_received_on_icap")
}

//is204Allowed is a func to check if ICAP request has the header "204 : Allowed" or not
func (i *ICAPRequest) is204Allowed() bool {
	Is204Allowed := false
	if _, exist := i.req.Header["Allow"]; exist &&
		i.req.Header.Get("Allow") == strconv.Itoa(utils.NoModificationStatusCodeStr) {
		Is204Allowed = true
	}
	return Is204Allowed
}

//shadowService is a func to apply the shadow service
func (i *ICAPRequest) shadowService() {
	zLog.Debug().Dur("duration", i.elapsed).Str("value", "processing not required for this request").
		Msgf("shadow_service_is_enabled")
	if i.Is204Allowed { // following RFC3507, if the request has Allow: 204 header, it is to be checked and if it doesn't exists, return the request as it is to the ICAP client, https://tools.ietf.org/html/rfc3507#section-4.6
		i.elapsed = time.Since(i.logger.LogStartTime)
		zLog.Debug().Dur("duration", i.elapsed).Str("value", "the file won't be modified").
			Msgf("request_received_on_icap_with_header_204")
		i.w.WriteHeader(utils.NoModificationStatusCodeStr, nil, false)
	} else {
		if i.req.Method == "REQMOD" {
			i.w.WriteHeader(utils.OkStatusCodeStr, i.req.Request, true)
			tempBody, _ := ioutil.ReadAll(i.req.Request.Body)
			i.w.Write(tempBody)
			i.req.Request.Body = io.NopCloser(bytes.NewBuffer(tempBody))
		} else if i.req.Method == "RESPMOD" {
			i.w.WriteHeader(utils.OkStatusCodeStr, i.req.Response, true)
			tempBody, _ := ioutil.ReadAll(i.req.Response.Body)
			i.w.Write(tempBody)
			i.req.Response.Body = io.NopCloser(bytes.NewBuffer(tempBody))
		}
	}
}

//getEnabledMethods is a func get all enable method of a specific service
func (i *ICAPRequest) getEnabledMethods() string {
	var allMethods []string
	if readValues.ReadValuesBool(i.serviceName + ".resp_mode") {
		allMethods = append(allMethods, "RESPMOD")
	}
	if readValues.ReadValuesBool(i.serviceName + ".req_mode") {
		allMethods = append(allMethods, "REQMOD")
	}
	if len(allMethods) == 1 {
		return allMethods[0]
	}
	return allMethods[0] + ", " + allMethods[1]
}

//optionsMode is a func to return an ICAP response in OPTIONS mode
func (i *ICAPRequest) optionsMode() {
	//optionsModeRemote(vendor, req, w, appCfg, zlogger)
	i.h.Set("Methods", i.getEnabledMethods())
	i.h.Set("Allow", "204")
	// Add preview if preview_enabled is true in config
	if i.appCfg.PreviewEnabled == true {
		if pb, _ := strconv.Atoi(i.appCfg.PreviewBytes); pb >= 0 {
			i.h.Set("Preview", i.appCfg.PreviewBytes)
		}
	}
	i.h.Set("Transfer-Preview", utils.Any)
	i.w.WriteHeader(http.StatusOK, nil, false)
}
