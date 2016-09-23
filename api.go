package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/femot/openmap-tools/opm"
	"github.com/femot/openmap-tools/util"
	"github.com/femot/pgoapi-go/api"
	"github.com/pogodevorg/POGOProtos-go"
)

var ErrBusy = errors.New("All our minions are busy")

func listenAndServe() {
	// Setup routes
	http.HandleFunc("/s", statusHandler)
	http.HandleFunc("/q", requestHandler)
	http.HandleFunc("/c", cacheHandler)
	// Start listening
	log.Fatal(http.ListenAndServe(settings.ListenAddr, nil))
}

func cacheHandler(w http.ResponseWriter, r *http.Request) {
	var objects []opm.MapObject
	// Check method
	if r.Method != "POST" {
		writeApiResponse(w, false, errors.New("Wrong method").Error(), objects)
		return
	}
	// Get Latitude and Longitude
	lat, err := strconv.ParseFloat(r.FormValue("lat"), 64)
	if err != nil {
		writeApiResponse(w, false, err.Error(), objects)
		return
	}
	lng, err := strconv.ParseFloat(r.FormValue("lng"), 64)
	if err != nil {
		writeApiResponse(w, false, err.Error(), objects)
	}
	// Pokemon/Gym/Pokestop filter
	filter := make([]int, 0)
	if r.FormValue("p") != "" {
		filter = append(filter, opm.POKEMON)
	}
	if r.FormValue("s") != "" {
		filter = append(filter, opm.POKESTOP)
	}
	if r.FormValue("g") != "" {
		filter = append(filter, opm.GYM)
	}
	// If no filter is set -> show everything
	if len(filter) == 0 {
		filter = []int{opm.POKEMON, opm.POKESTOP, opm.GYM}
	}
	// Get objects from db
	objects, err = database.GetMapObjects(lat, lng, filter, settings.CacheRadius)
	if err != nil {
		writeApiResponse(w, false, "Failed to get MapObjects from DB", objects)
		log.Println(err)
		return
	}
	writeApiResponse(w, true, "", objects)
}

func requestHandler(w http.ResponseWriter, r *http.Request) {
	// Check method
	if r.Method != "POST" {
		writeApiResponse(w, false, errors.New("Wrong method").Error(), nil)
		return
	}
	// Get Latitude and Longitude
	lat, err := strconv.ParseFloat(r.FormValue("lat"), 64)
	if err != nil {
		writeApiResponse(w, false, err.Error(), nil)
		return
	}
	lng, err := strconv.ParseFloat(r.FormValue("lng"), 64)
	if err != nil {
		writeApiResponse(w, false, err.Error(), nil)
	}
	// Get trainer from queue
	trainer, err := trainerQueue.Get(5 * time.Second)
	if err != nil {
		// Timeout -> try setup a new one
		p, err := database.GetProxy()
		if err != nil {
			writeApiResponse(w, false, ErrBusy.Error(), nil)
			return
		}
		a, err := database.GetAccount()
		if err != nil {
			database.ReturnProxy(p)
			writeApiResponse(w, false, ErrBusy.Error(), nil)
			return
		}
		trainer = util.NewTrainerSession(a, &api.Location{}, feed, crypto)
		trainer.SetProxy(p)
		status[trainer.Account.Username] = opm.StatusEntry{AccountName: trainer.Account.Username, ProxyId: trainer.Proxy.Id}
	}
	defer trainerQueue.Queue(trainer, time.Duration(settings.ScanDelay)*time.Second)
	// Create context
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	trainer.Context = ctx
	log.Printf("Using %s for request\t(%.6f,%.6f)", trainer.Account.Username, lat, lng)
	// Perform scan
	mapObjects, err := getMapResult(trainer, lat, lng)
	// Error handling
	retrySuccess := false
	// Handle proxy death
	if err != nil && err == api.ErrProxyDead {
		trainer.Proxy.Dead = true
		var p opm.Proxy
		p, err = database.GetProxy()
		if err == nil {
			trainer.SetProxy(p)
			status[trainer.Account.Username] = opm.StatusEntry{AccountName: trainer.Account.Username, ProxyId: trainer.Proxy.Id}
			// Retry with new proxy
			mapObjects, err = getMapResult(trainer, lat, lng)
			retrySuccess = err == nil
		} else {
			delete(status, trainer.Account.Username)
			database.ReturnAccount(trainer.Account)
			log.Println("No proxies available")
			writeApiResponse(w, false, ErrBusy.Error(), nil)
			return
		}
	}
	// Account problems
	if err != nil {
		errString := err.Error()
		if strings.Contains(errString, "Your username or password is incorrect") || err == api.ErrAccountBanned || err.Error() == "Empty response" || strings.Contains(errString, "not yet active") {
			log.Printf("Account %s banned", trainer.Account.Username)
			trainer.Account.Banned = true
			database.UpdateAccount(trainer.Account)
			delete(status, trainer.Account.Username)
		}
	}
	// Just retry when this error comes
	if err == api.ErrInvalidPlatformRequest {
		mapObjects, err = getMapResult(trainer, lat, lng)
	}
	// Final error check
	if err != nil && !retrySuccess {
		writeApiResponse(w, false, err.Error(), nil)
		return
	}
	//Save to db
	for _, o := range mapObjects {
		database.AddMapObject(o)
	}
	writeApiResponse(w, true, "", mapObjects)
}

func writeApiResponse(w http.ResponseWriter, ok bool, e string, response []opm.MapObject) {
	w.Header().Add("Access-Control-Allow-Origin", settings.AllowOrigin)
	w.Header().Add("Content-Type", "application/json")

	if e != "" && e != ErrBusy.Error() {
		e = "Scan failed"
	}

	r := opm.ApiResponse{Ok: ok, Error: e, MapObjects: response}
	err := json.NewEncoder(w).Encode(r)
	if err != nil {
		log.Println(err)
	}
}

func getMapResult(trainer *util.TrainerSession, lat float64, lng float64) ([]opm.MapObject, error) {
	// Set location
	trainer.MoveTo(&api.Location{Lat: lat, Lon: lng})
	// Login trainer
	err := trainer.Login()
	if err == api.ErrInvalidAuthToken {
		trainer.ForceLogin = true
		err = trainer.Login()
	}
	if err != nil {
		if err != api.ErrProxyDead {
			log.Printf("Login error (%s):\n\t\t%s\n", trainer.Account.Username, err.Error())
		}
		return nil, err
	}
	// Query api
	<-ticks
	mapObjects, err := trainer.GetPlayerMap()
	if err != nil && err != api.ErrNewRPCURL {
		if err != api.ErrProxyDead {
			log.Printf("Error getting map objects (%s):\n\t\t%s\n", trainer.Account.Username, err.Error())
		}
		return nil, err
	}
	// Parse and return result
	return parseMapObjects(mapObjects), nil
}

func parseMapObjects(r *protos.GetMapObjectsResponse) []opm.MapObject {
	objects := make([]opm.MapObject, 0)
	// Cells
	for _, c := range r.MapCells {
		// Pokemon
		for _, p := range c.WildPokemons {
			expiry := time.Now().Add(time.Duration(p.TimeTillHiddenMs) * time.Millisecond).Unix()
			objects = append(objects, opm.MapObject{
				Type:      opm.POKEMON,
				Id:        strconv.FormatUint(p.EncounterId, 36),
				PokemonId: int(p.PokemonData.PokemonId),
				Lat:       p.Latitude,
				Lng:       p.Longitude,
				Expiry:    expiry,
			})
		}
		// Forts
		for _, f := range c.Forts {
			switch f.Type {
			case protos.FortType_CHECKPOINT:
				if f.LureInfo != nil {
					// Lured pokemon found!
					objects = append(objects, opm.MapObject{
						Type:      opm.POKEMON,
						Id:        strconv.FormatUint(f.LureInfo.EncounterId, 36),
						PokemonId: int(f.LureInfo.ActivePokemonId),
						Lat:       f.Latitude,
						Lng:       f.Longitude,
						Expiry:    f.LureInfo.LureExpiresTimestampMs / 1000,
					})
				}
				objects = append(objects, opm.MapObject{
					Type:  opm.POKESTOP,
					Id:    f.Id,
					Lat:   f.Latitude,
					Lng:   f.Longitude,
					Lured: f.ActiveFortModifier != nil,
				})
			case protos.FortType_GYM:
				objects = append(objects, opm.MapObject{
					Type: opm.GYM,
					Id:   f.Id,
					Lat:  f.Latitude,
					Lng:  f.Longitude,
					Team: int(f.OwnedByTeam),
				})
			}
		}
	}
	return objects
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	if r.FormValue("secret") != settings.Secret {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "nope")
		return
	}

	list := make([]opm.StatusEntry, 0)
	for _, v := range status {
		list = append(list, v)
	}
	w.WriteHeader(http.StatusOK)
	w.Header().Add("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}
