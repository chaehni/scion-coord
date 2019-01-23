// Copyright 2017 ETH Zurich
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/astaxie/beego/orm"
	"github.com/gorilla/mux"
	"github.com/netsec-ethz/scion-coord/config"
	"github.com/netsec-ethz/scion-coord/controllers"
	"github.com/netsec-ethz/scion-coord/controllers/middleware"
	"github.com/netsec-ethz/scion-coord/email"
	"github.com/netsec-ethz/scion-coord/models"
	"github.com/netsec-ethz/scion-coord/utility"
	"github.com/scionproto/scion/go/lib/addr"
	"github.com/scionproto/scion/go/lib/crypto"
	"github.com/scionproto/scion/go/lib/crypto/cert"
)

var (
	_, b, _, _      = runtime.Caller(0)
	currentPath     = filepath.Dir(b)
	scionCoordPath  = filepath.Dir(filepath.Dir(currentPath))
	localGenPath    = filepath.Join(scionCoordPath, "python", "local_gen.py")
	TempPath        = filepath.Join(scionCoordPath, "temp")
	githubPath      = filepath.Dir(filepath.Dir(scionCoordPath))
	scionPath       = filepath.Join(githubPath, "scionproto", "scion")
	scionUtilPath   = filepath.Join(scionCoordPath, "sub", "util")
	pythonPath      = filepath.Join(scionPath, "python")
	vagrantPath     = filepath.Join(scionCoordPath, "vagrant")
	auxFilesPath    = filepath.Join(scionCoordPath, "files")
	PackagePath     = config.PackageDirectory
	CertsPath       = "certs"
	BoxPackagePath  = filepath.Join(PackagePath, "SCIONBox")
	credentialsPath = filepath.Join(scionCoordPath, "credentials")
	EasyRSAPath     = filepath.Join(PackagePath, "easy-rsa")
	RSAKeyPath      = filepath.Join(EasyRSAPath, "keys")
	CACertPath      = filepath.Join(RSAKeyPath, "ca.crt")
	HeartBeatPeriod = time.Duration(config.HeartbeatPeriod)
	HeartBeatLimit  = time.Duration(config.HeartbeatLimit)
)

// TODO(mlegner): We need to find a better way to handle all the credential files.
func CredentialFile(isd addr.ISD, ending string) string {
	return filepath.Join(credentialsPath, fmt.Sprintf("ISD%d.%s", isd, ending))
}

func CoreCertFile(isd addr.ISD) string {
	return CredentialFile(isd, "crt")
}

func CoreSigKey(isd addr.ISD) string {
	return CredentialFile(isd, "key")
}

func TrcFile(isd addr.ISD) string {
	return CredentialFile(isd, "trc")
}

func UserPackageName(email string, isd addr.ISD, as addr.AS) string {
	return fmt.Sprintf("%s_%s", email, utility.IAFileName(isd, as))
}

func (asInfo *SCIONLabASInfo) UserPackageName() string {
	return UserPackageName(asInfo.LocalAS.UserEmail, asInfo.LocalAS.ISD, asInfo.LocalAS.ASID)
}

func (asInfo *SCIONLabASInfo) UserPackagePath() string {
	return filepath.Join(PackagePath, asInfo.UserPackageName())
}

type SCIONLabASController struct {
	controllers.HTTPController
}

type SCIONLabASInfo struct {
	IsNewConnection bool               // denotes whether this is a new user.
	IsVPN           bool               // denotes whether this is a VPN setup
	VPNServerIP     string             // IP of the VPN server
	VPNServerPort   uint16             // Port of the VPN server
	IP              string             // the public IP address of the SCIONLab AS
	LocalPort       uint16             // The port of the border router on the user side
	OldAP           string             // the previous SCIONLab AP to which the AS was connected
	RemoteIA        addr.IA            // the SCIONLab AP the AS connects to
	RemoteIP        string             // the IP address of the SCIONLab AP it connects to
	RemoteBRID      uint16             // ID of the border router in the SCIONLab AP
	RemotePort      uint16             // Port of the BR in the SCIONLab AP
	LocalAS         *models.SCIONLabAS // if exists, the DB object that belongs to this AS
	RemoteAS        *models.SCIONLabAS // the AP this AS connects to
}

type SCIONLabRequest struct {
	ASID      addr.AS `json:"asID"`
	UserEmail string  `json:"userEmail"`
	IsVPN     bool    `json:"isVPN"`
	IP        string  `json:"ip"`
	ServerIA  string  `json:"serverIA"`
	Label     string  `json:"label"`
	Type      uint8   `json:"type"`
	Port      uint16  `json:"port"`
}

type remappingError struct {
	err          error
	notifyAdmins bool
}

func newMappingError(notifyAdmins bool, format string, params ...interface{}) *remappingError {
	return &remappingError{err: fmt.Errorf(format, params...), notifyAdmins: notifyAdmins}
}
func (e *remappingError) Error() string {
	return e.err.Error()
}
func (e *remappingError) LogAndNotifyAppropriately(w http.ResponseWriter, format string, params ...interface{}) {
	if e.notifyAdmins {
		logAndSendErrorAndNotifyAdmins(w, format, params...)
	} else {
		logAndSendError(w, format, params...)
	}
}

// BadRequestAndLog writes a HTTP 400 error with the message and error, and prints the same in the server log
func (s *SCIONLabASController) BadRequestAndLog(w http.ResponseWriter, err error, desc string, a ...interface{}) {
	msg := controllers.Verbosity(err, desc, a...)
	s.BadRequest(w, nil, msg)
	log.Print(msg)
}

func sendAlreadyCompressedFile(w http.ResponseWriter, filePath, fileNameInClient string) error {
	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("Error reading the file %v: %v", filePath, err)
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", "attachment; filename="+fileNameInClient)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
	return nil
}

// List of all ASes belonging to the account
func ownedASes(r *http.Request) (map[string]struct{}, error) {
	vars := mux.Vars(r)
	accountID := vars["account_id"]
	asesList, err := models.FindSCIONLabASesByAccountID(accountID)
	if err != nil {
		return nil, err
	}
	ases := make(map[string]struct{})
	for _, as := range asesList {
		ases[as] = struct{}{}
	}
	return ases, nil
}

// Check if the account is the owner of the specified IA
func checkAuthorization(r *http.Request, ia string) (addr.IA, error) {
	IA, err := utility.IAFromString(ia)
	if err != nil {
		return IA, err
	}
	// ensure apIA is always non file format:
	ia = IA.String()
	ases, err := ownedASes(r)
	if err != nil {
		return IA, err
	}
	_, ourAS := ases[ia]
	if !ourAS {
		return IA, fmt.Errorf("The AS %v does not belong to the specified account", ia)
	}
	return IA, nil
}

// This generates a new AS for the user if they do not have too many already
func (s *SCIONLabASController) GenerateNewSCIONLabAS(w http.ResponseWriter, r *http.Request) {
	_, uSess, err := middleware.GetUserSession(r)
	if err != nil {
		log.Printf("Error getting the user session: %v", err)
		s.Forbidden(w, err, "Error getting the user session")
		return
	}
	ases, err := models.FindSCIONLabASesByUserEmail(uSess.Email)
	if err != nil {
		log.Printf("Error looking up current SCIONLabASes for %v: %v", uSess.Email, err)
		s.Error500(w, err, "Error looking up current SCIONLabASes")
		return
	}
	maxASes := config.MaxASes(uSess.IsAdmin)
	if len(ases) >= maxASes {
		s.Forbidden(w, nil, "You can currently only create %v ASes", maxASes)
		return
	}
	asID, err := s.getNewSCIONLabASID()
	if err != nil {
		log.Printf("Error generating new ASID for %v: %v", uSess.Email, err)
		s.Error500(w, err, "Error generating new ASID")
		return
	}
	newAS := models.SCIONLabAS{
		UserEmail:   uSess.Email,
		StartPort:   config.BRStartPort,
		ASID:        asID,
		Type:        models.VM,
		Credits:     config.VirtualCreditStartCredits,
		ConfVersion: 0,
		Branch:      config.TestingCoordinatorBranch,
	}
	if err := newAS.Insert(); err != nil {
		log.Printf("Error inserting new AS for %v: %v", uSess.Email, err)
		s.Error500(w, err, "Error inserting new AS into database")
		return
	}
	fmt.Fprintf(w, "A new AS with ID %v has been generated for you. "+
		"Please use the form below to configure it.", asID)
	return
}

func generateGenForAS(asInfo *SCIONLabASInfo) error {
	var err error
	// Generate topology file
	if err = generateTopologyFile(asInfo); err != nil {
		return fmt.Errorf("Error generating topology file: %v", err)
	}
	// Generate local gen
	if err = generateLocalGen(asInfo); err != nil {
		return fmt.Errorf("Error generating local config: %v", err)
	}
	// preserve certificates (don't use new ones if we had certs already)
	if err = preserveCerts(asInfo); err != nil {
		return fmt.Errorf("Error reusing existing certificates: %v", err)
	}

	// Generate VPN config if this is a VPN setup
	if asInfo.IsVPN {
		if err = generateVPNConfig(asInfo); err != nil {
			return fmt.Errorf("Error generating VPN config: %v", err)
		}
	}
	if err = addAuxiliaryFiles(asInfo); err != nil {
		return fmt.Errorf("Error adding auxiliary files to the package: %v", err)
	}
	// Add account id and secret to gen directory
	err = createUserLoginConfiguration(asInfo)
	if err != nil {
		return fmt.Errorf("Error generating user credential files: %v", err)
	}
	// Package the SCIONLab AS configuration
	err = packageConfiguration(asInfo)
	if err != nil {
		return fmt.Errorf("Error packaging SCIONLabAS configuration: %v", err)
	}
	return nil
}

// The main handler function to generates a SCIONLab AS for the given user.
// If successful, the front-end will initiate the downloading of the tarball.
func (s *SCIONLabASController) ConfigureSCIONLabAS(w http.ResponseWriter, r *http.Request) {
	// Parse the arguments
	slReq, err := s.parseRequestParameters(r)
	if err != nil {
		s.BadRequestAndLog(w, err, "Error parsing the parameters")
		return
	}
	// check if there is already a create or update in progress
	if err := s.canConfigure(slReq.UserEmail, slReq.ASID); err != nil {
		log.Printf("Error checking pending create or update for user %v: %v", slReq.UserEmail, err)
		s.Error500(w, err, "Error checking pending create or update")
		return
	}
	// Target SCIONLab ISD and AS to connect to is determined by config file
	asInfo, err := s.getSCIONLabASInfo(slReq)
	if err != nil {
		log.Printf("Error getting SCIONLabASInfo: %v", err)
		s.Error500(w, err, "Error getting SCIONLabASInfo")
		return
	}
	asInfo.LocalAS.ConfVersion++ // we are creating a new configuration
	// Remove all existing files from UserPackagePath
	os.RemoveAll(asInfo.UserPackagePath() + "/")
	// generate the gen folder:
	err = generateGenForAS(asInfo)
	if err != nil {
		log.Print(err)
		s.Error500(w, err, "Error generating the configuration")
		return
	}

	// Persist the relevant data into the DB
	if err = s.updateDB(asInfo); err != nil {
		log.Printf("Error updating DB tables: %v", err)
		s.Error500(w, err, "Error updating DB tables")
		return
	}

	message := "Your SCIONLab AS will be activated within a few minutes. " +
		"You will receive an email confirmation as soon as the process is complete."
	fmt.Fprintln(w, message)
}

// Parses the JSON payload of the request and checks if it is valid
func (s *SCIONLabASController) parseRequestParameters(r *http.Request) (
	slReq SCIONLabRequest, err error) {
	// Get user session
	_, uSess, err := middleware.GetUserSession(r)
	if err != nil {
		log.Printf("Error getting the user session: %v", err)
		return
	}
	// parse the JSON coming from the client
	decoder := json.NewDecoder(r.Body)
	// check if the parsing succeeded
	if err = decoder.Decode(&slReq); err != nil {
		return
	}

	// set the email address
	slReq.UserEmail = uSess.Email
	// check that ServerIA is not empty
	if slReq.ServerIA == "" {
		err = errors.New("server IA cannot be empty")
		return
	}
	// check that valid type is given
	if slReq.Type != models.VM && slReq.Type != models.Dedicated {
		err = errors.New("invalid AS type given")
		return
	}
	// check that IP address is not empty for nonVPN setup
	if !slReq.IsVPN && slReq.IP == "" {
		err = fmt.Errorf("IP address cannot be empty for non-VPN setup. User: %v", slReq.UserEmail)
		return
	}
	return
}

// Check if the user's AS is already in the process of being created or updated.
func (s *SCIONLabASController) canConfigure(userEmail string, asID addr.AS) error {
	as, err := models.FindSCIONLabASByUserEmailAndASID(userEmail, asID)
	if err != nil {
		return err
	}
	if (as.Status == models.Active) || (as.Status == models.Inactive) {
		if as.Type == models.Infrastructure {
			return errors.New("cannot modify infrastructure ASes")
		}
		return nil
	}
	return errors.New("the given AS has a pending update request")
}

// Checks that no other AS exists with same IP address
// TODO(mlegner): This condition is more strict than necessary and should be loosened
func (s *SCIONLabASController) checkRequest(slReq SCIONLabRequest) error {
	if slReq.IsVPN {
		return nil
	}
	ases, err := models.FindSCIONLabASesByIP(slReq.IP)
	if err != nil {
		return fmt.Errorf("error looking up ASes: %v", err)
	}
	l := len(ases)

	if l == 0 || l == 1 && ases[0].ASID == slReq.ASID {
		return nil
	}

	return fmt.Errorf("there exists another AS with the same public IP address %v", slReq.IP)
}

// Populates and returns a SCIONLabASInfo struct, which contains the necessary information
// to create the SCIONLab AS configuration.
func (s *SCIONLabASController) getSCIONLabASInfo(slReq SCIONLabRequest) (*SCIONLabASInfo, error) {
	newConnection := true
	var brID, vpnPort uint16
	var ip, remoteIP, vpnIP, oldAP string
	var cn models.ConnectionInfo
	// See if this user already has an AS
	as, err := models.FindSCIONLabASByUserEmailAndASID(slReq.UserEmail, slReq.ASID)
	if err != nil {
		return nil, fmt.Errorf("error looking up SCIONLab AS for user %v: %v",
			slReq.UserEmail, err)
	}
	cns, err := as.GetJoinConnectionInfo()
	if err != nil {
		return nil, fmt.Errorf("error looking up connections of SCIONLab AS for user %v: %v",
			slReq.UserEmail, err)
	}
	// look for an existing connection to the same AP:
	cns = models.OnlyCurrentConnections(cns)
	for _, cn = range cns {
		oldAP = utility.IAString(cn.NeighborISD, cn.NeighborAS)
		if oldAP == slReq.ServerIA {
			newConnection = false
			brID = cn.NeighborBRID
			break
		}
	}

	remoteIA, err := addr.IAFromString(slReq.ServerIA)
	if err != nil {
		return nil, err
	}

	remoteAS, err := models.FindSCIONLabASByIAString(slReq.ServerIA)
	if err != nil {
		return nil, fmt.Errorf("error while retrieving AttachmentPoint %v: %v", slReq.ServerIA, err)
	}

	// Different settings depending on whether it is a VPN or standard setup
	if slReq.IsVPN {
		if !remoteAS.AP.HasVPN {
			return nil, errors.New("the AttachmentPoint does not have an openVPN server running")
		}
		if !newConnection && cn.IsVPN {
			ip = cn.LocalIP
		} else {
			ip, err = remoteAS.GetFreeVPNIP()
			if err != nil {
				return nil, err
			}
			log.Printf("New VPN IP to be assigned to user %v: %v", slReq.UserEmail, ip)
		}
		remoteIP = remoteAS.AP.VPNIP
		vpnIP = remoteAS.PublicIP
		vpnPort = remoteAS.AP.VPNPort
	} else {
		ip = slReq.IP
		remoteIP = remoteAS.PublicIP
		log.Printf("IP address of AttachementPoint = %v", remoteIP)
	}

	if int(brID) < config.ReservedBRsInfrastructure {
		brID, err = remoteAS.GetFreeBRID()
		if err != nil {
			return nil, err
		}
		log.Printf("New BR ID to be assigned to user %v: %v", slReq.UserEmail, brID)
	}

	if slReq.Port > 0 {
		as.StartPort = slReq.Port
	}
	as.Type = slReq.Type
	if as.Status == models.Inactive {
		as.Status = models.Create
	} else {
		as.Status = models.Update
	}
	as.PublicIP = slReq.IP
	as.ISD = remoteIA.I
	as.Label = slReq.Label

	return &SCIONLabASInfo{
		IsNewConnection: newConnection,
		IsVPN:           slReq.IsVPN,
		RemoteIA:        remoteIA,
		IP:              ip,
		LocalPort:       as.StartPort,
		OldAP:           oldAP,
		RemoteIP:        remoteIP,
		RemoteBRID:      brID,
		RemotePort:      remoteAS.GetPortNumberFromBRID(brID),
		VPNServerIP:     vpnIP,
		VPNServerPort:   vpnPort,
		LocalAS:         as,
		RemoteAS:        remoteAS,
	}, nil
}

func getSCIONLabASInfoFromDB(conn *models.Connection) (*SCIONLabASInfo, error) {
	conn.JoinAS = conn.GetJoinAS()
	conn.RespondAP = conn.GetRespondAP()
	conn.RespondAP.AS = conn.GetRespondAS()
	asInfo := SCIONLabASInfo{
		IsNewConnection: false,
		IsVPN:           conn.IsVPN,
		RemoteIA:        conn.RespondAP.AS.IA(),
		IP:              conn.JoinIP,
		LocalPort:       conn.JoinAS.StartPort,
		OldAP:           "",
		RemoteIP:        conn.RespondIP,
		RemoteBRID:      conn.RespondBRID,
		RemotePort:      conn.RespondAP.AS.GetPortNumberFromBRID(conn.RespondBRID),
		VPNServerIP:     conn.RespondAP.AS.PublicIP,
		VPNServerPort:   conn.RespondAP.VPNPort,
		LocalAS:         conn.JoinAS,
		RemoteAS:        conn.RespondAP.AS,
	}
	return &asInfo, nil
}

// Updates the relevant database tables related to SCIONLab AS creation.
func (s *SCIONLabASController) updateDB(asInfo *SCIONLabASInfo) error {
	userEmail := asInfo.LocalAS.UserEmail
	if asInfo.IsNewConnection {
		// flag the old connections for deletion:
		if asInfo.OldAP != "" {
			asInfo.LocalAS.FlagAllConnectionsToAPToBeDeleted(asInfo.OldAP)
		}
		// update the Connections table
		newCn := models.Connection{
			JoinIP:        asInfo.IP,
			RespondIP:     asInfo.RemoteIP,
			JoinAS:        asInfo.LocalAS,
			RespondAP:     asInfo.RemoteAS.AP,
			JoinBRID:      1,
			RespondBRID:   asInfo.RemoteBRID,
			Linktype:      models.Parent,
			IsVPN:         asInfo.IsVPN,
			JoinStatus:    models.Active,
			RespondStatus: models.Create,
		}
		if err := newCn.Insert(); err != nil {
			return fmt.Errorf("error inserting new Connection for user %v: %v",
				userEmail, err)
		}
		// update the AS database table
		if err := asInfo.LocalAS.Update(); err != nil {
			newCn.Delete()
			return fmt.Errorf("error updating SCIONLabAS database table for user %v: %v",
				userEmail, err)
		}
	} else {
		// we had found an existing connection to the same AP.
		// Update the Connections Table
		cns, err := asInfo.LocalAS.GetJoinConnectionInfoToAS(asInfo.RemoteIA.String())
		if err != nil {
			return fmt.Errorf("error finding existing connection of user %v: %v",
				userEmail, err)
		}
		cns = models.OnlyCurrentConnections(cns)
		if len(cns) != 1 {
			// we've failed our assertion that there's only one active connection. Complain.
			return fmt.Errorf("Error updating SCIONLabAS AS %v to AP %v: we expected 1 connection and found %v",
				asInfo.LocalAS.IAString(), asInfo.RemoteIA, len(cns))
		}
		cn := cns[0]
		cn.BRID = 1
		cn.IsVPN = asInfo.IsVPN
		cn.LocalIP = asInfo.IP
		cn.NeighborIP = asInfo.RemoteIP
		cn.NeighborStatus = asInfo.LocalAS.Status
		cn.Status = models.Active
		if err := asInfo.LocalAS.UpdateASAndConnectionFromJoinConnInfo(&cn); err != nil {
			return fmt.Errorf("error updating database tables for user %v: %v",
				userEmail, err)
		}
	}
	return nil
}

// Provides a new AS ID for the newly created SCIONLab AS AS.
// TODO(mlegner): Should we maybe use the lowest unused ID instead?
// TODO: this function is too expensive: we retrieve all AS and convert them to ASInfo, only to
// ensure the ID is bigger than the biggest of them! FIXME now!! (reviewer, tell me to fix it now)
func (s *SCIONLabASController) getNewSCIONLabASID() (addr.AS, error) {
	ases, err := models.FindAllASInfos()
	if err != nil {
		return 0, err
	}
	// Base AS ID for SCIONLab is set in config file
	asID := config.BaseASID
	for _, as := range ases {
		if as.ASID > asID {
			asID = as.ASID
		}
	}
	return asID + 1, nil
}

// Generates the path to the temporary topology file
func (asInfo *SCIONLabASInfo) topologyFile() string {
	iaForFile := utility.IAFileName(asInfo.LocalAS.ISD, asInfo.LocalAS.ASID)
	return filepath.Join(TempPath, iaForFile+"_topology.json")
}

// Generates the topology file for the SCIONLab AS AS. It uses the template file
// simple_config_topo.tmpl under templates folder in order to populate and generate the
// JSON file.
func generateTopologyFile(asInfo *SCIONLabASInfo) error {
	log.Printf("Generating topology file for SCIONLab AS")
	t, err := template.ParseFiles("templates/simple_config_topo.tmpl")
	if err != nil {
		return fmt.Errorf("error parsing topology template config for user %v: %v",
			asInfo.LocalAS.UserEmail, err)
	}
	f, err := os.Create(asInfo.topologyFile())
	if err != nil {
		return fmt.Errorf("error creating topology file config for user %v: %v",
			asInfo.LocalAS.UserEmail, err)
	}
	localIP := config.LocalhostIP
	if asInfo.LocalAS.Type == models.VM {
		localIP = config.VMLocalIP
	}
	localIA := asInfo.LocalAS.IAString()

	// Topology file parameters
	data := map[string]string{
		"IP":           asInfo.IP,
		"BIND_IP":      asInfo.LocalAS.BindIP(asInfo.IsVPN, asInfo.IP),
		"ISD_ID":       fmt.Sprintf("%d", asInfo.LocalAS.ISD),
		"AS_ID":        asInfo.LocalAS.ASID.FileFmt(),
		"LOCAL_ISDAS":  localIA,
		"LOCAL_ADDR":   localIP,
		"LOCAL_PORT":   strconv.Itoa(int(asInfo.LocalPort)),
		"TARGET_ISDAS": asInfo.RemoteIA.String(),
		"REMOTE_ADDR":  asInfo.RemoteIP,
		"REMOTE_PORT":  strconv.Itoa(int(asInfo.RemotePort)),
	}
	if err = t.Execute(f, data); err != nil {
		return fmt.Errorf("error executing topology template file for user %v: %v",
			asInfo.LocalAS.UserEmail, err)
	}
	f.Close()
	return nil
}

// TODO(mlegner): Add option specifying already existing keys and certificates
// Creates the local gen folder of the SCIONLab AS AS. It calls a Python wrapper script
// located under the python directory. The script uses SCION's and SCION-WEB's library
// functions in order to generate the certificate, AS keys etc.
func generateLocalGen(asInfo *SCIONLabASInfo) error {
	log.Printf("Creating gen folder for SCIONLab AS")
	isd := asInfo.LocalAS.ISD
	asID := asInfo.LocalAS.ASID
	userEmail := asInfo.LocalAS.UserEmail
	log.Printf("Calling create local gen. ISD-ID: %v, AS-ID: %v, UserEmail: %v", isd, asID,
		userEmail)
	signingAs, haveit := config.SigningASes[isd]
	if !haveit {
		return fmt.Errorf("signing AS for ISD %v not configured", isd)
	}

	cmd := exec.Command("python3", localGenPath,
		"--topo_file="+asInfo.topologyFile(), "--user_id="+asInfo.UserPackageName(),
		"--joining_ia="+utility.IAStringStandard(isd, asID),
		"--core_ia="+utility.IAStringStandard(isd, signingAs),
		"--core_sign_priv_key_file="+CoreSigKey(isd),
		"--core_cert_file="+CoreCertFile(isd),
		"--trc_file="+TrcFile(isd),
		"--package_path="+PackagePath,
		"--no-prometheus")
	pyPaths := []string{}
	if pythonPath != "" {
		pyPaths = []string{pythonPath}
	}
	if scionPath != "" {
		pyPaths = append(pyPaths, scionPath)
	}
	if scionUtilPath != "" {
		pyPaths = append(pyPaths, scionUtilPath)
	}
	pyPath := strings.Join(pyPaths, ":")
	fmt.Println("PYTHONPATH:", pyPath)
	os.Setenv("PYTHONPATH", pyPath)
	cmd.Env = os.Environ()
	cmdOut, _ := cmd.StdoutPipe()
	cmdErr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("generate local gen command could not start for user %v: %v",
			userEmail, err)
	}
	// read stdout and stderr
	stdOutput, _ := ioutil.ReadAll(cmdOut)
	errOutput, _ := ioutil.ReadAll(cmdErr)
	fmt.Printf("STDOUT generateLocalGen: %s\n", stdOutput)
	fmt.Printf("ERROUT generateLocalGen: %s\n", errOutput)
	if len(errOutput) != 0 {
		return fmt.Errorf("generate local gen command reported errors: %s", errOutput)
	}
	return nil
}

func addAuxiliaryFiles(asInfo *SCIONLabASInfo) error {
	userEmail := asInfo.LocalAS.UserEmail
	userPackagePath := asInfo.UserPackagePath()
	log.Printf("Adding auxiliary files to the package %v", asInfo.UserPackageName())
	if asInfo.LocalAS.Type == models.Dedicated {
		dedicatedAuxFiles := filepath.Join(auxFilesPath, "dedicated_box")
		err := utility.CopyPath(dedicatedAuxFiles, userPackagePath)
		if err != nil {
			return fmt.Errorf("failed to copy files for user %v: src: %v, dst: %v, %v",
				userEmail, dedicatedAuxFiles, userPackagePath, err)
		}
	}
	return nil
}

// the generated AS will have new certificates. Only if they have a higher version that our cache
// we will keep them. Otherwise we will replace them with our cache's
func preserveCerts(asInfo *SCIONLabASInfo) error {
	// this functions copies "certs" and "keys" from "src" to all
	// "dstSubDirs" in "dst"
	packageName := asInfo.UserPackageName()
	log.Printf("Trying to preserve certificates for %s", packageName)
	src := filepath.Join(PackagePath, CertsPath, packageName)
	dst := filepath.Join(asInfo.UserPackagePath(),
		"gen",
		fmt.Sprintf("ISD%d", asInfo.LocalAS.ISD),
		fmt.Sprintf("AS%s", asInfo.LocalAS.IA().A.FileFmt()),
	)
	dstSubDirs := []string{"."} // we assume we'll copy to the cache
	if _, err := os.Stat(dst); os.IsNotExist(err) {
		return fmt.Errorf("Path should exist but does not (%s): %v", dst, err)
	}
	// get the highest version from the new AS:
	newCertsPath := filepath.Join(dst, "endhost", "certs")
	newCerts, err := filepath.Glob(filepath.Join(newCertsPath, "*.crt"))
	if err != nil {
		return fmt.Errorf("Could not read %s: %v", newCertsPath, err)
	}
	maxNewVersion := -1
	for _, f := range newCerts {
		re := regexp.MustCompile(`ISD.+-AS.+-(V\d+).crt$`)
		groups := re.FindStringSubmatch(f)
		if len(groups) == 2 {
			v, err := strconv.Atoi(groups[1][1:])
			if err != nil {
				log.Printf(`skipping version "%s": cannot parse: %v`, groups[1][1:], err)
				continue
			}
			if v > maxNewVersion {
				maxNewVersion = v
			}
		}
	}
	if maxNewVersion < 0 {
		return fmt.Errorf("Could not find a valid certificate version for AS in %s", packageName)
	}
	// get the highest version from the cache:
	maxExistingVersion := -1
	if _, err := os.Stat(src); os.IsNotExist(err) {
		// we don't have a cache yet. Create it with no version yet
		log.Printf("No certificate cache, creating empty one")
		err = os.MkdirAll(src, 0700)
		if err != nil {
			return fmt.Errorf("Error calling mkdir on %s: %v", dst, err)
		}
	} else if err == nil {
		// we have a cache. Find out the newest version
		log.Printf("Certificate cache found.")
		existingVersions, err := filepath.Glob(filepath.Join(src, "V*"))
		if err != nil {
			return fmt.Errorf("Could not read %s: %v", newCertsPath, err)
		}
		for _, d := range existingVersions {
			d = filepath.Base(d)
			v, err := strconv.Atoi(d[1:])
			if err != nil {
				log.Printf(`skipping version "%s": cannot parse: %v`, d[1:], err)
				continue
			}
			if v > maxExistingVersion {
				maxExistingVersion = v
			}
		}
	} else {
		return fmt.Errorf("Error when stat on path %s: %v", src, err)
	}
	log.Printf("Cert. versions. Existing is %d, generated is %d", maxExistingVersion, maxNewVersion)
	if maxNewVersion > maxExistingVersion {
		// new generated certificate version is newer. Swap src and dst
		src, dst = dst, src
		dst = filepath.Join(dst, fmt.Sprintf("V%d", maxNewVersion))
		err = os.MkdirAll(dst, 0700)
		if err != nil {
			return fmt.Errorf("Error calling mkdir on %s: %v", dst, err)
		}
		src = filepath.Join(src, "endhost") // use the certs from endhost
	} else {
		// "normal" case, from cache to AS folder. Find the dstSubDirs
		src = filepath.Join(src, fmt.Sprintf("V%d", maxExistingVersion))
		fileInfos, err := ioutil.ReadDir(dst)
		if err != nil {
			return fmt.Errorf("Could not read directory %s: %v", dst, err)
		}
		dstSubDirs = nil
		for _, f := range fileInfos {
			name := f.Name()
			if f.IsDir() && strings.HasPrefix(name, "br") ||
				strings.HasPrefix(name, "cs") ||
				strings.HasPrefix(name, "bs") ||
				strings.HasPrefix(name, "ps") ||
				name == "endhost" {
				dstSubDirs = append(dstSubDirs, name)
			}
		}
	}
	// in any case, copy from src to dst:
	for _, dir := range []string{"certs", "keys"} {
		srcItem := filepath.Join(src, dir)
		_, err := os.Stat(srcItem)
		if err != nil {
			return fmt.Errorf("Could not read the directory %s", srcItem)
		}
		for _, dstSubDir := range dstSubDirs {
			dstItem := filepath.Join(dst, dstSubDir, dir)
			err = os.MkdirAll(dstItem, 0700)
			if err != nil {
				return fmt.Errorf("Could not mkdir part of the destination path %s: %v", dstItem, err)
			}
			err = utility.CopyPath(srcItem, dstItem)
			if err != nil {
				return fmt.Errorf("Could not fully copy %s to %s: %v", srcItem, dstItem, err)
			}
		}
	}
	log.Printf("Preserve certificates completed")
	return nil
}

// TODO(mlegner): Add README for Dedicated setup
// Packages the SCIONLab AS configuration as a tarball and returns the name of the
// generated file.
func packageConfiguration(asInfo *SCIONLabASInfo) error {
	log.Printf("Packaging SCIONLab AS")
	userEmail := asInfo.LocalAS.UserEmail
	userPackageName := asInfo.UserPackageName()
	userPackagePath := asInfo.UserPackagePath()

	// Only copy all vagrant-related files if this is a VM-type AS
	if asInfo.LocalAS.Type == models.VM {
		vagrantDir, err := os.Open(vagrantPath)
		if err != nil {
			return fmt.Errorf("failed to open directory. Path: %v, %v", vagrantPath, err)
		}
		objects, err := vagrantDir.Readdir(-1)
		if err != nil {
			return fmt.Errorf("failed to read directory contents. Path: %v, %v", vagrantPath, err)
		}
		for _, obj := range objects {
			src := filepath.Join(vagrantPath, obj.Name())
			dst := filepath.Join(userPackagePath, obj.Name())
			if !obj.IsDir() {
				if err = utility.CopyFile(src, dst); err != nil {
					return fmt.Errorf("failed to copy files for user %v: src: %v, dst: %v, %v",
						userEmail, src, dst, err)
				}
			}
		}
		portForwarding := ""
		if !asInfo.IsVPN {
			portForwarding = fmt.Sprintf("config.vm.network \"forwarded_port\", "+
				"guest: %[1]v, host: %[1]v, protocol: \"udp\"", asInfo.LocalPort)
		}
		data := struct {
			ASID           string
			PortForwarding string
		}{
			ASID:           asInfo.LocalAS.ASID.FileFmt(),
			PortForwarding: portForwarding,
		}
		if err := utility.FillTemplateAndSave("templates/Vagrantfile.tmpl",
			data, filepath.Join(userPackagePath, "Vagrantfile")); err != nil {
			return err
		}
	}

	cmd := exec.Command("tar", "czf", userPackageName+".tar.gz", userPackageName)
	cmd.Dir = PackagePath
	err := cmd.Start()
	if err == nil {
		err = cmd.Wait()
	}
	if err != nil {
		return fmt.Errorf("failed to create SCIONLabAS tarball for user %v: %v", userEmail, err)
	}

	return nil
}

func createUserLoginConfiguration(asInfo *SCIONLabASInfo) error {
	log.Printf("Creating user authentication files")
	userEmail := asInfo.LocalAS.UserEmail
	acc, err := models.FindAccountByUserEmail(userEmail)
	if err != nil {
		return fmt.Errorf("failed to find account for email %s: %v", userEmail, err)
	}

	userGenDir := filepath.Join(asInfo.UserPackagePath(), "gen")

	accountId := []byte(acc.AccountID)
	err = ioutil.WriteFile(filepath.Join(userGenDir, "account_id"), accountId, 0644)
	if err != nil {
		return fmt.Errorf("failed to write account ID to file: %v", err)
	}

	accountSecret := []byte(acc.Secret)
	err = ioutil.WriteFile(filepath.Join(userGenDir, "account_secret"), accountSecret, 0644)
	if err != nil {
		return fmt.Errorf("failed to write account secret to file: %v", err)
	}

	ia := utility.IAFileName(asInfo.LocalAS.ISD, asInfo.LocalAS.ASID)
	iaString := []byte(ia)
	err = ioutil.WriteFile(filepath.Join(userGenDir, "ia"), iaString, 0644)
	if err != nil {
		return fmt.Errorf("failed to write IA to file: %v", err)
	}

	confVersion := strconv.FormatUint(uint64(asInfo.LocalAS.ConfVersion), 10)
	err = ioutil.WriteFile(filepath.Join(userGenDir, "coord_conf.ver"), []byte(confVersion), 0644)
	if err != nil {
		return fmt.Errorf("failed to write configuration version to file: %v", err)
	}

	if config.TestingCoordinatorBranch != "" {
		// this is a testing Coordinator, write down its URL
		err = ioutil.WriteFile(filepath.Join(userGenDir, "coord_url"), []byte(config.HTTPHostAddress), 0644)
		if err != nil {
			return fmt.Errorf("failed to write Coordinator URL to file: %v", err)
		}
	}

	return nil
}

// API end-point to serve the generated SCIONLab AS configuration tarball.
func (s *SCIONLabASController) ReturnTarball(w http.ResponseWriter, r *http.Request) {
	_, uSess, err := middleware.GetUserSession(r)
	if err != nil {
		log.Printf("Error getting the user session: %v", err)
		s.Forbidden(w, err, "Error getting the user session")
		return
	}
	vars := mux.Vars(r)
	asIDstr := vars["as_id"]
	asID, err := utility.ASIDFromString(asIDstr)
	if err != nil {
		s.BadRequestAndLog(w, nil, err.Error())
		return
	}
	as, err := models.FindSCIONLabASByUserEmailAndASID(uSess.Email, asID)
	if err != nil || as.Status == models.Inactive || as.Status == models.Remove {
		s.BadRequestAndLog(w, nil, "No active configuration found for user %v, asID %v", uSess.Email, asID)
		return
	}

	fileName := UserPackageName(uSess.Email, as.ISD, as.ASID) + ".tar.gz"
	filePath := filepath.Join(PackagePath, fileName)
	err = sendAlreadyCompressedFile(w, filePath, "scion_lab_"+fileName)
	if err != nil {
		s.Error500(w, err, "Error reading tarball")
		return
	}
}

func logAndSendError(w http.ResponseWriter, errorMsgFmt string, parms ...interface{}) string {
	errorMsg := fmt.Sprintf(errorMsgFmt, parms...)
	log.Print(errorMsg)
	dict := make(map[string]interface{})
	dict["error"] = true
	dict["msg"] = errorMsg
	utility.SendJSONError(dict, w)
	return errorMsg
}

func logAndSendErrorAndNotifyAdmins(w http.ResponseWriter, errorMsgFmt string, parms ...interface{}) {
	msg := logAndSendError(w, errorMsgFmt, parms...)
	email.SendEmailToAdmins("ERROR in remap", msg)
}

func getASAndCheckChallenge(r *http.Request, ia string, verifyChallenge bool) (
	*models.SCIONLabAS, map[string]interface{}, *remappingError) {
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, nil, newMappingError(false, "Could not read JSON in the request for IA %v: %v", ia, err)
	}
	request := make(map[string]interface{})
	json.Unmarshal(body, &request)
	as, err := models.FindSCIONLabASByIAString(ia)
	if err != nil {
		return nil, nil, newMappingError(true, "Could not find AS with IA %v", ia)
	}
	if !verifyChallenge {
		return as, request, nil
	}

	challenge, havechallenge := request["challenge"]
	challengeSolution, haveanswer := request["challenge_solution"]
	if !havechallenge || !haveanswer {
		return nil, nil, newMappingError(true, `JSON missing "challenge" or "challenge_solution", IA `, ia)
	}
	challengeInDB, err := as.GetRemapChallenge()
	if err != nil {
		return nil, nil, newMappingError(true, "Error getting challenge for IA %v: %v", ia, err)
	}
	if challenge != challengeInDB {
		return nil, nil, newMappingError(true, "Challenge stored and received don't match. IA %v", ia)
	}
	// verify challenge solution
	receivedSignature, err := base64.StdEncoding.DecodeString(challengeSolution.(string))
	if err != nil {
		return nil, nil, newMappingError(true, "Cannot decode the answer to the challenge, IA: %v", ia)
	}
	challengeAsBytes, err := base64.StdEncoding.DecodeString(challenge.(string))
	if err != nil {
		return nil, nil, newMappingError(true, "Internal error: cannot decode the stored challenge, IA: %v", ia)
	}
	err = verifySignatureFromAS(as, challengeAsBytes, receivedSignature)
	if err != nil {
		return nil, nil, newMappingError(true, "Cannot verify signature for IA %v: %v", ia, err)
	}
	return as, request, nil
}

func verifySignatureFromAS(as *models.SCIONLabAS, thingToSign, receivedSignature []byte) error {
	path := filepath.Join(PackagePath,
		UserPackageName(as.UserEmail, as.ISD, as.ASID),
		"gen",
		fmt.Sprintf("ISD%d", as.ISD),
		fmt.Sprintf("AS%d", as.ASID),
		fmt.Sprintf("bs%d-%d-1", as.ISD, as.ASID),
		"certs")
	var chain *cert.Chain
	fileInfos, err := ioutil.ReadDir(path)
	if err != nil {
		return err
	}

	possibleCerts := []string{}
	for _, f := range fileInfos {
		if !f.IsDir() && strings.HasSuffix(strings.ToLower(f.Name()), ".crt") {
			possibleCerts = append(possibleCerts, f.Name())
		}
	}
	if len(possibleCerts) < 1 {
		return fmt.Errorf("Cannot find any .crt file for IA %v", as.IAString())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(possibleCerts)))
	path = filepath.Join(path, possibleCerts[0])
	chainBytes, err := ioutil.ReadFile(path)
	if err != nil {
		return err
	}
	chain, err = cert.ChainFromRaw(chainBytes, false)
	if err != nil || chain == nil {
		msg := fmt.Sprintf("ERROR in Coordinator: cannot load the public certificate for AS %s : %v", as.IAString(), err)
		email.SendEmailToAdmins("ERROR in remap", msg)
		return errors.New(msg)
	}
	publicKey := chain.Leaf.SubjectSignKey
	err = crypto.Verify(thingToSign, receivedSignature, publicKey, crypto.Ed25519)
	return err
}

// remapASIDComputeNewGenFolder creates a new gen folder using a valid remapped ID
// e.g. 17-ffaa:0:1 . This does not change IDs in the DB but recomputes topologies and certificates.
// After finishing, there will be a new tgz file ready to download using the mapped ID.
func remapASIDComputeNewGenFolder(as *models.SCIONLabAS) (*addr.IA, error) {
	ia := utility.MapOldIAToNewOne(as.ISD, as.ASID)
	if ia.I == 0 || ia.A == 0 {
		return nil, fmt.Errorf("Invalid source address to map: (%d, %d)", as.ISD, as.ASID)
	}
	// replace IDs in the AS entry, but don't save the version to the DB:
	as.ISD = ia.I
	as.ASID = ia.A
	// generate the tarball with +1, as it is a new configuration. But don't save to DB
	as.ConfVersion++
	err := computeNewGenFolder(as)
	ia = as.IA()
	return &ia, err
}

// computeNewGenFolder takes a SCIONLabAS model and (re)creates a tarbal and configuration folder
func computeNewGenFolder(as *models.SCIONLabAS) error {
	ia := as.IA()
	// retrieve connection:
	conns, err := as.GetJoinNotRemovedConnections()
	if err != nil {
		return err
	}
	if len(conns) != 1 {
		err = fmt.Errorf("User AS should have only 1 connection. %s has %d", ia, len(conns))
		return err
	}
	conn := conns[0]
	asInfo, err := getSCIONLabASInfoFromDB(conn)
	asInfo.LocalAS = as
	if err != nil {
		return err
	}
	// finally, generate the gen folder:
	os.RemoveAll(asInfo.UserPackagePath())
	return generateGenForAS(asInfo)
}

// RemapASIdentityChallengeAndSolution returns the challenge the AS should solve if said AS has to map the identity.
func (s *SCIONLabASController) RemapASIdentityChallengeAndSolution(w http.ResponseWriter, r *http.Request) {
	answeringChallenge := false
	if r.Method == http.MethodPost {
		answeringChallenge = true
	}
	answer := make(map[string]interface{})
	answer["error"] = false
	vars := mux.Vars(r)
	ia, err := utility.NormalizeIAString(vars["ia"])
	if err != nil {
		logAndSendError(w, err.Error())
		return
	}
	log.Printf("Remap request from %v. Solving challenge? %v", ia, answeringChallenge)
	as, _, mapErr := getASAndCheckChallenge(r, ia, answeringChallenge)
	if mapErr != nil {
		mapErr.LogAndNotifyAppropriately(w, mapErr.Error())
		return
	}
	if !answeringChallenge {
		needsRemap := !as.AreIDsFromScionLab()
		answer["pending"] = needsRemap
		challenge, err := as.GetRemapChallenge()
		if err != nil && needsRemap {
			logAndSendErrorAndNotifyAdmins(w, err.Error())
			return
		}
		answer["challenge"] = challenge
		utility.SendJSON(answer, w)
		log.Printf("Remap: sent challenge for %v", ia)
		return
	}
	answer["ia"], err = remapASIDComputeNewGenFolder(as)
	if err != nil {
		logAndSendErrorAndNotifyAdmins(w, "ERROR in Coordinator: while mapping the ID, cannot generate a gen folder for the AS %s : %s", ia, err.Error())
		return
	}
	err = utility.SendJSON(answer, w)
	if err != nil {
		log.Printf("Error during JSON marshaling: %v", err)
		s.Error500(w, err, "Error during JSON marshaling")
		return
	}
	log.Printf("Remap: finished computing new GEN.")
}

// RemapASDownloadGen will accept a JSON object containing the query from a user AS to obtain the
// new gen folder for a new ID after the remap on the IDs during the summer of 2018
func (s *SCIONLabASController) RemapASDownloadGen(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	ia, err := utility.NormalizeIAString(vars["ia"])
	if err != nil {
		logAndSendError(w, err.Error())
		return
	}
	log.Printf("Remap: request download GEN from %v", ia)
	as, _, mapErr := getASAndCheckChallenge(r, ia, true)
	if mapErr != nil {
		mapErr.LogAndNotifyAppropriately(w, mapErr.Error())
		return
	}
	mappedIA := utility.MapOldIAToNewOne(as.ISD, as.ASID)
	fileName := UserPackageName(as.UserEmail, mappedIA.I, mappedIA.A) + ".tar.gz"
	filePath := filepath.Join(PackagePath, fileName)
	err = sendAlreadyCompressedFile(w, filePath, "scion_lab_"+fileName)
	if err != nil {
		logAndSendError(w, "Error reading the tarball. FileName: %v, %v", fileName, err)
		return
	}
}

// RemapASConfirmStatus receives confirmation from a user AS that they applied the mapping.
// The confirmation is writen in the DB with a timestamp.
func (s *SCIONLabASController) RemapASConfirmStatus(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	ia, err := utility.NormalizeIAString(vars["ia"])
	if err != nil {
		logAndSendError(w, err.Error())
		return
	}
	log.Printf("Remap: confirming mapping for %v", ia)
	as, _, mapErr := getASAndCheckChallenge(r, ia, true)
	if mapErr != nil {
		mapErr.LogAndNotifyAppropriately(w, mapErr.Error())
		return
	}
	mappedIA := utility.MapOldIAToNewOne(as.ISD, as.ASID)
	as.ISD = mappedIA.I
	as.ASID = mappedIA.A
	answer := make(map[string]interface{})
	answer["pending"] = false
	answer["date"] = time.Now()
	// set its status to Create so the AP will create it:
	conns, err := as.GetJoinNotRemovedConnections()
	if err != nil {
		logAndSendError(w, err.Error())
		return
	}
	if len(conns) != 1 {
		logAndSendError(w, "User AS should have only 1 connection. %s has %d", ia, len(conns))
		return
	}
	conns[0].RespondStatus = models.Create
	err = conns[0].Update()
	if err != nil {
		logAndSendError(w, "Cannot update connection for AS %v: %v", ia, err)
		return
	}
	as.Status = models.Create
	// the expected version is +1 (we generated the tarball with +1). Write to DB
	as.ConfVersion++
	err = as.SetMappingStatusAndSave(answer)
	if err != nil {
		answer["error"] = true
		msg := fmt.Sprintf("Could not update mapping status for AS: %v", err)
		answer["msg"] = msg
		log.Print(msg)
		utility.SendJSONError(answer, w)
		return
	}
	log.Printf("Updated mapping for AS %v -> %v", ia, mappedIA)
}

// The handler function to remove a SCIONLab AS for the given user.
// If successful, it will return a 200 status with an empty response.
func (s *SCIONLabASController) RemoveSCIONLabAS(w http.ResponseWriter, r *http.Request) {
	_, uSess, err := middleware.GetUserSession(r)
	if err != nil {
		log.Printf("Error getting the user session: %v", err)
		s.Error500(w, err, "Error getting the user session")
	}
	userEmail := uSess.Email
	vars := mux.Vars(r)
	asIDStr := vars["as_id"]
	asID, err := utility.ASIDFromString(asIDStr)
	if err != nil {
		log.Println(err.Error())
		s.Error500(w, err, "Bad format")
		return
	}
	// check if there is an active AS which can be removed
	canRemove, as, cn, err := s.canRemove(userEmail, asID)
	if err != nil {
		log.Printf("Error checking if your AS can be removed for user %v: %v", userEmail, err)
		s.Error500(w, err, "Error checking if AS can be removed")
		return
	}
	if !canRemove {
		s.BadRequestAndLog(w, nil, "You currently do not have an active SCIONLab AS.")
		return
	}
	as.ConfVersion++
	as.Status = models.Remove
	cn.NeighborStatus = models.Remove
	cn.Status = models.Inactive
	if err := as.UpdateASAndConnectionFromJoinConnInfo(cn); err != nil {
		log.Printf("Error marking AS and Connection as removed for user %v: %v",
			userEmail, err)
		s.Error500(w, err, "Error marking AS and Connection as removed")
		return
	}
	log.Printf("Marked removal of SCIONLabAS of user %v.", userEmail)
	fmt.Fprintln(w, "Your AS will be removed within the next few minutes. "+
		"You will receive a confirmation email as soon as the removal is complete.")
}

// Check if the user's AS is already removed or in the process of being removed.
// Can remove a AS only if it is in the Active state.
func (s *SCIONLabASController) canRemove(userEmail string, asID addr.AS) (bool, *models.SCIONLabAS,
	*models.ConnectionInfo, error) {
	as, err := models.FindSCIONLabASByUserEmailAndASID(userEmail, asID)
	if err != nil {
		if err == orm.ErrNoRows {
			return false, nil, nil, nil
		} else {
			return false, nil, nil, err
		}
	}
	if as.Status == models.Active {
		if as.Type == models.Infrastructure {
			return false, nil, nil, errors.New("cannot remove infrastructure ASes")
		}
		cns, err := as.GetJoinConnectionInfo()
		if err != nil {
			return false, nil, nil, fmt.Errorf("error looking up connections: %v", err)
		}
		cns = models.OnlyCurrentConnections(cns)
		l := len(cns)
		if err != nil || l == 0 {
			return false, nil, nil, err
		}
		if l > 1 {
			return false, nil, nil, fmt.Errorf("AS %v has currently %v connections", asID, l)
		}
		// TODO: we support only one active connection per AS
		return true, as, &cns[0], nil
	}
	return false, nil, nil, nil
}

// Reads the IA parameter from the URL and returns the associated SCIONLabAS if it belongs to the
// correct account and an error otherwise
func (s *SCIONLabASController) getIAParameter(r *http.Request) (*models.SCIONLabAS, error) {
	ia, err := checkAuthorization(r, r.URL.Query().Get("IA"))
	if err != nil {
		return nil, err
	}
	return models.FindSCIONLabASByIAInt(ia.I, ia.A)
}

// QueryUpdateBranch API for SCIONLabASes to query which git branch they should use for updates
func (s *SCIONLabASController) QueryUpdateBranch(w http.ResponseWriter, r *http.Request) {
	log.Printf("API Call for queryUpdateBranch = %v", r.URL.Query())
	as, err := s.getIAParameter(r)
	if err != nil {
		s.BadRequestAndLog(w, nil, err.Error())
		return
	}
	s.Plain(as.Branch, w, r)
}

// ConfirmUpdate API for SCIONLabASes to report a successful update
// E.g. curl -X POST -I http://localhost:8080/api/as/confirmUpdate/someid/some_secret?IA=1-ffaa_1_1
func (s *SCIONLabASController) ConfirmUpdate(w http.ResponseWriter, r *http.Request) {
	log.Printf("API Call for confirmUpdate = %v", r.URL.Query())
	as, err := s.getIAParameter(r)
	if err != nil {
		s.BadRequestAndLog(w, nil, err.Error())
		return
	}
	as.Update() // just to set the Updated field to Now()
	w.WriteHeader(http.StatusNoContent)
}

// GetASData will return
// 400 if there was an error or the local version is higher than the stored one
// 304 (not modified) if the AS already obtained the latest AS configuration
// 205 (reset content) if the AS needs to delete its gen folder
// otherwise 200 with the TGZ the AS can automatically untar and use. It will include all what the users
// download from the web page directly (VPN, README, etc).
// If the force=true (or force=1) flag was specified, ignore versions and assume client's is older
// E.g. curl -s -D - --output myfile.tgz http://localhost:8080/api/as/getASData/someid/some_secret/9-ffaa_1_1?local_version=1
func (s *SCIONLabASController) GetASData(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	log.Printf("API call for GetASDAta as_id=%s, URL = %v", vars["ia"], r.URL.Query())
	ia, err := utility.NormalizeIAString(vars["ia"])
	if err != nil {
		s.BadRequestAndLog(w, nil, err.Error())
		return
	}
	as, err := models.FindSCIONLabASByIAString(ia)
	if err != nil {
		s.BadRequestAndLog(w, err, "Cannot find AS with given IA %s", ia)
		return
	}
	ia = as.IAString() // because we get the AS ignoring the ISD part, the real ia could be different
	forceFlag, _ := strconv.ParseBool(r.URL.Query().Get("force"))
	str := r.URL.Query().Get("local_version")
	v64, err := strconv.ParseUint(str, 10, 32)
	if err != nil {
		log.Printf("WARNING! version string (%s) cannot be converted to a 32 uint. Using 0 as version", str)
	}
	localVersion := uint(v64)
	log.Printf("IA %s, current version %d, local version is %d", ia, as.ConfVersion, localVersion)
	if !forceFlag && localVersion > as.ConfVersion {
		messageToAdmins := fmt.Sprintf("The AS with IA %s reported a possibly wrong local version "+
			"> AS.ConvVersion (%d > %d)", ia, localVersion, as.ConfVersion)
		err = email.SendEmailToAdmins("ERROR During GetASData", messageToAdmins)
		if err != nil {
			fmt.Printf("ERROR (again): could not send email to admins: %v", err)
		}
		// try to recover by sending the configuration or the code to remove the AS:
		forceFlag = true
	}
	if !forceFlag && localVersion == as.ConfVersion {
		w.WriteHeader(http.StatusNotModified)
	} else if as.Status == models.Remove {
		w.WriteHeader(http.StatusResetContent)
	} else {
		err = computeNewGenFolder(as)
		if err != nil {
			s.BadRequestAndLog(w, nil, "We failed (re)creating the tarball file for IA %s: %v", ia, err)
			return
		}
		fileName := UserPackageName(as.UserEmail, as.ISD, as.ASID) + ".tar.gz"
		filePath := filepath.Join(PackagePath, fileName)
		err = sendAlreadyCompressedFile(w, filePath, "scion_lab_"+fileName)
		if err != nil {
			s.BadRequestAndLog(w, nil, "Error reading the tarball. FileName: %v: %v", fileName, err)
			return
		}
	}
}
