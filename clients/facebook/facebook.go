package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/attic-labs/noms/Godeps/_workspace/src/golang.org/x/oauth2"
	"github.com/attic-labs/noms/Godeps/_workspace/src/golang.org/x/oauth2/facebook"
	"github.com/attic-labs/noms/clients/util"
	"github.com/attic-labs/noms/d"
	"github.com/attic-labs/noms/dataset"
	"github.com/attic-labs/noms/types"
)

const maxProcs = 25

var (
	albumIDFlag       = flag.String("album-id", "", "Import a specific album, identified by id")
	apiKeyFlag        = flag.String("api-key", "", "API keys for Facebook can be created at https://developers.facebook.com/apps/")
	apiKeySecretFlag  = flag.String("api-key-secret", "", "API keys for Facebook can be created at https://developers.facebook.com/apps/")
	authHTTPClient    *http.Client
	cachingHTTPClient *http.Client
	ds                *dataset.Dataset
	forceAuthFlag     = flag.Bool("force-auth", false, "Force re-authentication")
	quietFlag         = flag.Bool("quiet", false, "Don't print progress information")
	start             time.Time
)

func main() {
	flag.Usage = picasaUsage
	dsFlags := dataset.NewFlags()
	flag.Parse()
	cachingHTTPClient = util.CachingHttpClient()

	if *apiKeyFlag == "" || *apiKeySecretFlag == "" || cachingHTTPClient == nil {
		flag.Usage()
		return
	}

	ds = dsFlags.CreateDataset()
	if ds == nil {
		flag.Usage()
		return
	}
	defer ds.Close()

	var currentUser *User
	if commit, ok := ds.MaybeHead(); ok {
		currentUserRef := commit.Value().(RefOfUser)
		cu := currentUserRef.TargetValue(ds.Store())
		currentUser = &cu
	}

	var refreshToken string
	authHTTPClient, refreshToken = doAuthentication(currentUser)

	// set start after authentication so we don't count that time
	start = time.Now()

	var user *User
	if *albumIDFlag != "" {
		newUser := getSingleAlbum(*albumIDFlag)
		if currentUser != nil {
			user = mergeInCurrentAlbums(currentUser, newUser)
		} else {
			user = newUser
		}
	} else {
		user = getAlbums()
	}

	printStats(user)

	*user = user.SetRefreshToken(refreshToken)
	userRef := types.WriteValue(*user, ds.Store())
	fmt.Printf("userRef: %s\n", userRef)
	_, err := ds.Commit(NewRefOfUser(userRef))
	d.Exp.NoError(err)
}

func picasaUsage() {
	credentialSteps := `How to create Facebook API credentials:
  1) Go to https://developers.facebook.com/apps/
  2) From the “Select a project” pull down menu, choose “Create a project...”
  3) Fill in the “Project name” field (e.g. Aatic Facebook Importer), agree to the terms
  4) Put your website's url as "http://localhost:63000/"
     `

	fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, "\n%s\n\n", credentialSteps)
}

func getUser() User {
	uj := UserJSON{}
        callFacebookAPI(authHTTPClient, "v2.5/me", &uj)
	u := UserDef{Id: uj.ID, Name: uj.Name}.New(ds.Store())
        return u
}

func getSingleAlbum(albumID string) *User {
	amj := AlbumMetadataJSON{}
	path := fmt.Sprintf("v2.5/%d", albumID)
	callFacebookAPI(authHTTPClient, path, &amj)

        u := getUser()
	albums := NewMapOfStringToAlbum(ds.Store())
	albums = getAlbum(0, amj.ID, amj.Name, albums)

	types.WriteValue(albums, ds.Store())
	u = u.SetAlbums(albums)
	return &u
}

func getAlbums() *User {
	alj := AlbumListJSON{}
	callFacebookAPI(authHTTPClient, "v2.5/me/albums", &alj)
        fmt.Printf("%+v", alj)
	if !*quietFlag {
		fmt.Printf("Found %d albums\n", len(alj.Data))
	}
	albums := NewMapOfStringToAlbum(ds.Store())
        user := getUser()
	for i, entry := range alj.Data {
		albums = getAlbum(i, entry.ID, entry.Name, albums)
	}

	types.WriteValue(albums, ds.Store())
	user = user.SetAlbums(albums)
	return &user
}

func getAlbum(albumIndex int, albumId string, albumTitle string, albums MapOfStringToAlbum) MapOfStringToAlbum {
	a := AlbumDef{Id: albumId, Title: albumTitle}.New(ds.Store())
        /*
	remotePhotoRefs := getRemotePhotoRefs(&a, albumIndex)
	r := types.WriteValue(*remotePhotoRefs, ds.Store())
	a = a.SetPhotos(NewRefOfSetOfRefOfRemotePhoto(r))
        */
	return albums.Set(a.Id(), a)
}

/*
func getRemotePhotoRefs(album *Album, albumIndex int) *SetOfRefOfRemotePhoto {
	if album.NumPhotos() == 0 {
		return nil
	}
	remotePhotoRefs := NewSetOfRefOfRemotePhoto(ds.Store())
	if !*quietFlag {
		fmt.Printf("Album #%d: %q contains %d photos... ", albumIndex, album.Title(), album.NumPhotos())
	}
	for startIndex, foundPhotos := 0, true; uint64(album.NumPhotos()) > remotePhotoRefs.Len() && foundPhotos; startIndex += 1000 {
		foundPhotos = false
		aj := AlbumPhotosJSON{}
		path := fmt.Sprintf("user/default/albumid/%s?alt=json&max-results=1000", album.Id())
		if startIndex > 0 {
			path = fmt.Sprintf("%s%s%d", path, "&start-index=", startIndex)
		}
		callFacebookAPI(authHTTPClient, path, &aj)
		for _, e := range aj.Feed.Entry {
			foundPhotos = true
			tags := splitTags(e.MediaGroup.Tags.V)
			height, _ := strconv.Atoi(e.Height.V)
			width, _ := strconv.Atoi(e.Width.V)
			size := SizeDef{Height: uint32(height), Width: uint32(width)}
			sizes := MapOfSizeToStringDef{}
			sizes[size] = e.Content.Src
			geoPos := toGeopos(e.Geo.Point.Pos.V)
			p := RemotePhotoDef{
				Id:          e.ID.V,
				Title:       e.Title.V,
				Geoposition: geoPos,
				Url:         e.Content.Src,
				Sizes:       sizes,
				Tags:        tags,
			}.New(ds.Store())
			r := types.WriteValue(p, ds.Store())
			remotePhotoRefs = remotePhotoRefs.Insert(NewRefOfRemotePhoto(r))
		}
	}

	if !*quietFlag {
		fmt.Printf("fetched %d, elapsed time: %.2f secs\n", remotePhotoRefs.Len(), time.Now().Sub(start).Seconds())
	}
	return &remotePhotoRefs
}
*/

func printStats(user *User) {
	if !*quietFlag {
		numPhotos := uint64(0)
		albums := user.Albums()
		albums.IterAll(func(id string, album Album) {
			setOfRefOfPhotos := album.Photos().TargetValue(ds.Store())
			numPhotos = numPhotos + setOfRefOfPhotos.Len()
		})

		fmt.Printf("Imported %d album(s), %d photo(s), time: %.2f\n", albums.Len(), numPhotos, time.Now().Sub(start).Seconds())
	}
}

func mergeInCurrentAlbums(curUser *User, newUser *User) *User {
	albums := curUser.Albums()
	newUser.Albums().IterAll(func(id string, a Album) {
		albums = albums.Set(id, a)
	})
	*newUser = newUser.SetAlbums(albums)
	return newUser
}

func doAuthentication(currentUser *User) (c *http.Client, rt string) {
	if !*forceAuthFlag && currentUser != nil {
		rt = currentUser.RefreshToken()
		c = tryRefreshToken(rt)
	}
	if c == nil {
		c, rt = facebookOAuth()
	}
	return c, rt
}

func tryRefreshToken(rt string) *http.Client {
	var c *http.Client

	if rt != "" {
		t := oauth2.Token{}
		conf := baseConfig("")
		ct := "application/x-www-form-urlencoded"
		body := fmt.Sprintf("client_id=%s&client_secret=%s&grant_type=refresh_token&refresh_token=%s", *apiKeyFlag, *apiKeySecretFlag, rt)
		r, err := cachingHTTPClient.Post(facebook.Endpoint.TokenURL, ct, strings.NewReader(body))
		d.Chk.NoError(err)
		if r.StatusCode == 200 {
			buf, err := ioutil.ReadAll(r.Body)
			d.Chk.NoError(err)
			json.Unmarshal(buf, &t)
			c = conf.Client(oauth2.NoContext, &t)
		}
	}
	return c
}

func facebookOAuth() (*http.Client, string) {
	l, err := net.Listen("tcp", "localhost:63000")
	d.Chk.NoError(err)

	redirectURLAsNumbers := "http://" + l.Addr().String() + "/"
        redirectURL := strings.Replace(redirectURLAsNumbers, "127.0.0.1", "localhost", -1)
	conf := baseConfig(redirectURL)
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	state := fmt.Sprintf("%v", r.Uint32())
	u := conf.AuthCodeURL(state)

	// Redirect user to facebook's consent page to ask for permission
	// for the scopes specified above.
	fmt.Printf("Visit the following URL to authorize access to your Facebook data:\n%v\n", u)
	code, returnedState := awaitOAuthResponse(l)
	d.Chk.Equal(state, returnedState, "Oauth state is not correct")

	// Handle the exchange code to initiate a transport.
	t, err := conf.Exchange(oauth2.NoContext, code)
	d.Chk.NoError(err)

	client := conf.Client(oauth2.NoContext, t)

	return client, t.RefreshToken
}

func awaitOAuthResponse(l net.Listener) (string, string) {
	var code, state string

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("code") != "" && r.URL.Query().Get("state") != "" {
			code = r.URL.Query().Get("code")
			state = r.URL.Query().Get("state")
			w.Header().Add("content-type", "text/plain")
			fmt.Fprintf(w, "Authorized")
			l.Close()
		} else if err := r.URL.Query().Get("error"); err == "access_denied" {
			fmt.Fprintln(os.Stderr, "Request for authorization was denied.")
			os.Exit(0)
		} else if err := r.URL.Query().Get("error"); err != "" {
			l.Close()
			d.Chk.Fail(err)
		}
	})}
	srv.Serve(l)

	return code, state
}

func callFacebookAPI(client *http.Client, path string, response interface{}) {
	u := "https://graph.facebook.com/" + path
	req, err := http.NewRequest("GET", u, nil)
	d.Chk.NoError(err)

	resp, err := client.Do(req)
	d.Chk.NoError(err)

	msg := func() string {
		body := &bytes.Buffer{}
		_, err := io.Copy(body, resp.Body)
		d.Chk.NoError(err)
		return fmt.Sprintf("could not load %s: %d: %s", u, resp.StatusCode, body)
	}

	switch resp.StatusCode / 100 {
	case 4:
		d.Exp.Fail(msg())
	case 5:
		d.Chk.Fail(msg())
	}

	err = json.NewDecoder(resp.Body).Decode(response)
	d.Chk.NoError(err)
}

func baseConfig(redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     *apiKeyFlag,
		ClientSecret: *apiKeySecretFlag,
		RedirectURL:  redirectURL,
		Scopes:       []string{"user_photos"},
		Endpoint:     facebook.Endpoint,
	}
}

// General utility functions
func toGeopos(s string) GeopositionDef {
	s1 := strings.TrimSpace(s)
	geoPos := GeopositionDef{Latitude: 0.0, Longitude: 0.0}
	if s1 != "" {
		slice := strings.Split(s1, " ")
		lat, err := strconv.ParseFloat(slice[0], 32)
		if err == nil {
			geoPos.Latitude = float32(lat)
		}
		lon, err := strconv.ParseFloat(slice[1], 32)
		if err == nil {
			geoPos.Longitude = float32(lon)
		}
	}
	return geoPos
}

func toJSON(str interface{}) string {
	v, err := json.Marshal(str)
	d.Chk.NoError(err)
	return string(v)
}

func splitTags(s string) map[string]bool {
	tags := map[string]bool{}
	for _, s := range strings.Split(s, ",") {
		s1 := strings.Trim(s, " ")
		if s1 != "" {
			tags[s1] = true
		}
	}
	return tags
}