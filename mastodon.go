/// (c) Bernhard Tittelbach, 2019 - MIT License
package main

import (
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/ChimeraCoder/anaconda"
	"github.com/McKael/madon"
	"github.com/microcosm-cc/bluemonday"
	"github.com/spf13/viper"
)

/// code adapted from madonctl by McKael -- https://github.com/McKael/madonctl
/// Kudos!!
func madonMustInitClient() (client *madon.Client, err error) {

	appName := viper.GetString("app_name")
	instanceURL := viper.GetString("instance")
	appKey := viper.GetString("app_key")
	appSecret := viper.GetString("app_secret")
	appToken := viper.GetString("app_token")
	appScopes := viper.GetStringSlice("app_scopes")

	if instanceURL == "" {
		LogMadon_.Fatalln("madonInitClient:", "no instance provided")
	}

	LogMadon_.Println("madonInitClient:Instance: ", instanceURL)

	if appKey != "" && appSecret != "" {
		// We already have an app key/secret pair
		client, err = madon.RestoreApp(appName, instanceURL, appKey, appSecret, nil)
		client.SetUserToken(appToken, "", "", appScopes)
		if err != nil {
			return
		}
		// Check instance
		if _, err = client.GetCurrentInstance(); err != nil {
			LogMadon_.Fatalln("madonInitClient:", err, "could not connect to server with provided app ID/secret")
			return
		}
		LogMadon_.Println("madonInitClient:", "Using provided app ID/secret")
		return
	}

	if appKey != "" || appSecret != "" {
		LogMadon_.Fatalln("madonInitClient:", "Warning: provided app id/secrets incomplete -- registering again")
	}

	LogMadon_.Println("madonInitClient:", "Registered new application.")
	return
}

/// code adapted from madonctl by McKael -- https://github.com/McKael/madonctl
/// Kudos!!
func goSubscribeStreamOfTagNames(client *madon.Client, hashTagList []string, statusOutChan chan<- madon.Status) {
	streamName := "hashtag"
	evChan := make(chan madon.StreamEvent, 10)
	stop := make(chan bool)
	done := make(chan bool)
	var err error

	nTags := len(hashTagList)

	if nTags == 0 {
		LogMadon_.Fatalln("goSubscribeStreamOfTagNames: hashTagList cannot be empty")
	} else if nTags == 1 { // Usual case: Only 1 stream
		LogMadon_.Println(hashTagList)
		err = client.StreamListener(streamName, hashTagList[0], evChan, stop, done)
	} else { // Several streams

		tagEvCh := make([]chan madon.StreamEvent, nTags)
		tagDoneCh := make([]chan bool, nTags)
		for i, t := range hashTagList {
			LogMadon_.Printf("goSubscribeStreamOfTagNames: Launching listener for tag '%s'\n", t)
			tagEvCh[i] = make(chan madon.StreamEvent)
			tagDoneCh[i] = make(chan bool)
			e := client.StreamListener(streamName, t, tagEvCh[i], stop, tagDoneCh[i])
			if e != nil {
				if i > 0 { // Close previous connections
					close(stop)
				}
				err = e
				break
			}
			// Forward events to main ev channel
			go func(i int) {
				for {
					select {
					case _, ok := <-tagDoneCh[i]:
						if !ok { // end of streaming for this tag
							done <- true
							return
						}
					case ev := <-tagEvCh[i]:
						evChan <- ev
					}
				}
			}(i)
		}
	}

	if err != nil {
		LogMadon_.Fatalln("goSubscribeStreamOfTagNames:", err.Error())
	}

LISTENSTREAM:
	for {
		select {
		case v, ok := <-done:
			if !ok || v == true { // done is closed, end of streaming
				break LISTENSTREAM
			}
		case ev := <-evChan:
			switch ev.Event {
			case "error":
				if ev.Error != nil {
					if ev.Error == io.ErrUnexpectedEOF {
						LogMadon_.Println("goSubscribeStreamOfTagNames:", "The stream connection was unexpectedly closed")
						continue
					}
					LogMadon_.Printf("goSubscribeStreamOfTagNames: Error event: [%s] %s\n", ev.Event, ev.Error)
					continue
				}
				LogMadon_.Printf("goSubscribeStreamOfTagNames: Event: [%s]\n", ev.Event)
			case "update":
				s := ev.Data.(madon.Status)
				statusOutChan <- s
			case "notification", "delete":
				continue
			default:
				LogMadon_.Printf("goSubscribeStreamOfTagNames: Unhandled event: [%s] %T\n", ev.Event, ev.Data)
			}
		}
	}
	close(stop)
	close(evChan)
	close(statusOutChan)
	if err != nil {
		LogMadon_.Printf("goSubscribeStreamOfTagNames: Error: %s\n", err.Error())
		os.Exit(1)
	}
}

func getRelation(client *madon.Client, accID int64) (madon.Relationship, error) {
	relationshiplist, err := client.GetAccountRelationships([]int64{accID})
	if err != nil {
		return madon.Relationship{}, err
	}
	if len(relationshiplist) == 0 {
		return madon.Relationship{}, fmt.Errorf("AccountID not known, got empty result")
	}
	return relationshiplist[0], nil
}

func goBoostStati(client *madon.Client, stati_chan <-chan madon.Status) {
	for status := range stati_chan {
		LogMadon_.Printf("Boosting Status with ID %d published by @%s\n", status.ID, status.Account.Acct)
		client.ReblogStatus(status.ID)
	}
}

func goTweetStati(client *madon.Client, birdclient *anaconda.TwitterApi, stati_chan <-chan madon.Status) {
	tagstripper := bluemonday.NewPolicy()
	tagstripper.AllowElements("br")
	re_br2newline := regexp.MustCompile("<br[^/>]*/?>")
	for status := range stati_chan {
		LogMadon_.Printf("Tweeting Status with ID %d published by @%s\n", status.ID, status.Account.Acct)
		text := strings.TrimSpace(html.UnescapeString(re_br2newline.ReplaceAllString(tagstripper.Sanitize(status.Content), "\n")))
		twitter_media_ids := make([]string, 0, 4)
		for _, media := range status.MediaAttachments {
			if strings.Contains(media.Type, "image") {
				LogMadon_.Println("goTweetStati MediaAttachment:", media)
				imgurl := media.PreviewURL
				if media.RemoteURL != nil {
					imgurl = *media.RemoteURL
				}
				resp, err := http.Get(imgurl)
				if err != nil {
					LogMadon_.Println("Error getting MediaAttachment:", err)
					continue
				}
				if twitter_media_id, err := getImageForTweet(birdclient, resp.Body); err == nil { //respBody: io.ReaderCloser
					twitter_media_ids = append(twitter_media_ids, twitter_media_id)
				}
			} else {
				LogMadon_.Printf("Skipping download if media type %s", media.Type)
			}
		}
		LogMadon_.Println("Twitter MediaIds", twitter_media_ids)
		sendTweet(birdclient, text, twitter_media_ids)
	}
}

func goPrintStati(stati_chan <-chan madon.Status) {
	for status := range stati_chan {
		fmt.Printf("%+v\n", status)
	}
}
