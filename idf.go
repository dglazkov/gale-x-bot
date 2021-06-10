package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/dchest/stemmer/porter2"
)

var (
	spaceRegExp           *regexp.Regexp
	nonAlphaNumericRegExp *regexp.Regexp
)

const (
	CACHE_PATH     = ".cache"
	IDF_CACHE_PATH = "idf"
)

const DEBUG_PRINT = false

const AUTO_SAVE_INTERVAL = 5 * time.Minute

//This number should be incremetned every time the format of the JSON cache
//changes, so old caches will be discarded.
const IDF_JSON_FORMAT_VERSION = 2

func init() {
	spaceRegExp = regexp.MustCompile(`\s+`)
	nonAlphaNumericRegExp = regexp.MustCompile("[^a-zA-Z0-9]+")
}

type TFIDF struct {
	values   map[string]float64
	messages []*discordgo.Message
}

func (t *TFIDF) topStemmedWords(count int) []string {
	if count > len(t.values) {
		count = len(t.values)
	}
	var words []string
	for word := range t.values {
		words = append(words, word)
	}
	wordSorter := func(i int, j int) bool {
		return t.values[words[i]] > t.values[words[j]]
	}
	sort.Slice(words, wordSorter)
	return words[:count]
}

//AutoTopWords is like TopWords but sets the count to be no higher than maxCount
//but otherwise pick the count with the biggest tfidf dropoff.
func (t *TFIDF) AutoTopWords(maxCount int) []string {
	rawWords := t.topStemmedWords(maxCount)
	if DEBUG_PRINT {
		fmt.Printf("words: %v\n", rawWords)
	}

	maxDrop := 0.0
	maxDropIndex := 1
	lastValue := 0.0
	for i, rawWord := range rawWords {
		value := t.values[rawWord]
		if i == 0 {
			lastValue = value
			continue
		}
		diff := lastValue - value
		lastValue = value

		if DEBUG_PRINT {
			fmt.Printf("i: %v, word: %v, value: %v, drop: %v\n", i, rawWord, value, diff)
		}

		if diff > maxDrop {
			maxDrop = diff
			maxDropIndex = i
		}
	}

	if DEBUG_PRINT {
		fmt.Printf("maxDropIndex: %v, total words: %v\n", maxDropIndex, rawWords[:maxDropIndex])
	}

	return t.restemWords(rawWords[:maxDropIndex])
}

//TopWords returns count of the top words
func (t *TFIDF) TopWords(count int) []string {
	return t.restemWords(t.topStemmedWords(count))
}

//restemWords takes stemmed words and restems them based on the most common
//words in the collection.
func (t *TFIDF) restemWords(stemmedWords []string) []string {
	//stemmedWord --> restemmedWord -> count
	restemCandidates := make(map[string]map[string]int)
	for _, message := range t.messages {
		subRestemMap := restemsForContent(message.Content)
		for stemmedWord, subMap := range subRestemMap {
			if _, ok := restemCandidates[stemmedWord]; !ok {
				restemCandidates[stemmedWord] = make(map[string]int)
			}
			for originalWord, count := range subMap {
				restemCandidates[stemmedWord][originalWord] += count
			}
		}
	}

	result := make([]string, len(stemmedWords))
	for i, stemmedWord := range stemmedWords {
		candidates := restemCandidates[stemmedWord]
		//If we don't have a candidate, just leave as is
		if candidates == nil {
			result[i] = stemmedWord
			continue
		}
		bestCandidate := ""
		bestCount := 0
		for candidate, count := range candidates {
			if count <= bestCount {
				continue
			}
			bestCandidate = candidate
			bestCount = count
		}
		result[i] = bestCandidate
	}

	return result

}

func normalizeWord(input string) string {
	input = nonAlphaNumericRegExp.ReplaceAllString(input, "")
	input = porter2.Stemmer.Stem(input)
	return strings.ToLower(input)
}

func removeMentionsAndURLS(input string) string {
	//TODO: strip out markdown
	pieces := strings.Split(input, " ")
	var result []string
	for _, piece := range pieces {
		piece = strings.ToLower(piece)
		if strings.HasPrefix(piece, "https://") {
			continue
		}
		if strings.HasPrefix(piece, "http://") {
			continue
		}
		//Channel mentions look like <#837826557477126219>
		//User mentions look like <@!837476904742289429>
		if strings.HasPrefix(piece, "<") && strings.HasSuffix(piece, ">") {
			continue
		}
		result = append(result, piece)
	}
	return strings.Join(result, " ")
}

func wordsForString(input string) []string {
	input = strings.ReplaceAll(input, "-", " ")
	input = strings.ReplaceAll(input, "/", " ")
	return strings.Split(input, " ")
}

//restemsForContent returns the map of stemmedWord -> unstemmedWord --> count
func restemsForContent(input string) map[string]map[string]int {
	//Substantially recreated in extractWordsFromContent

	//normalize all spaces to just a single space
	input = spaceRegExp.ReplaceAllString(input, " ")
	input = removeMentionsAndURLS(input)

	result := make(map[string]map[string]int)

	for _, word := range wordsForString(input) {
		stemmedWord := normalizeWord(word)
		if stemmedWord == "" {
			continue
		}
		if _, ok := result[stemmedWord]; !ok {
			result[stemmedWord] = make(map[string]int)
		}
		result[stemmedWord][word] += 1
	}
	return result
}

func extractWordsFromContent(input string) []string {
	//Substantially recreated in restemsForContent

	//normalize all spaces to just a single space
	input = spaceRegExp.ReplaceAllString(input, " ")
	input = removeMentionsAndURLS(input)
	var result []string
	for _, word := range wordsForString(input) {
		word := normalizeWord(word)
		if word == "" {
			continue
		}
		result = append(result, word)
	}
	return result
}

type idfIndexJSON struct {
	DocumentCount int `json:"documentCount"`
	//Map of stemmedWord --> number of documents that have that word at least
	//once
	DocumentWordCounts map[string]int `json:"documentWordCounts"`
	FormatVersion      int            `json:"formatVersion"`
}

//IDFIndex stores information for calculating IDF of a thread. Get a new one
//from NewIDFIndex.
type IDFIndex struct {
	data    *idfIndexJSON
	guildID string
}

//IDFIndexForGuild returns either a preexisting IDF index from disk cache or a
//fresh one.z
func IDFIndexForGuild(guildID string, session *discordgo.Session) (*IDFIndex, error) {
	if result := LoadIDFIndex(guildID); result != nil {
		return result, nil
	}
	return BuildIDFIndex(guildID, session)
}

func LoadIDFIndex(guildID string) *IDFIndex {
	folderPath := filepath.Join(CACHE_PATH, IDF_CACHE_PATH)
	path := filepath.Join(folderPath, guildID+".json")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	blob, err := ioutil.ReadFile(path)
	if err != nil {
		fmt.Printf("couldn't read json file for %v: %v", guildID, err)
		return nil
	}
	var result idfIndexJSON
	if err := json.Unmarshal(blob, &result); err != nil {
		fmt.Printf("couldn't unmarshal json for %v: %v", guildID, err)
		return nil
	}
	if result.FormatVersion != IDF_JSON_FORMAT_VERSION {
		fmt.Printf("%v IDF cache file had old version %v, expected %v, discarding\n", guildID, result.FormatVersion, IDF_JSON_FORMAT_VERSION)
		return nil
	}
	fmt.Printf("Reloading guild IDF cachce for %v\n", guildID)
	return &IDFIndex{
		data:    &result,
		guildID: guildID,
	}
}

//This is the max limit in the discord API. Otherwise it defaults to 0
const MESSAGES_TO_FETCH = 100

func FetchAllMessagesForChannel(session *discordgo.Session, channel *discordgo.Channel) ([]*discordgo.Message, error) {
	//TODO: test this function
	var result []*discordgo.Message

	//We'll walk backwards, starting at lastMessageID, and fetching batches of
	//messages backwards until we run out.
	message, err := session.ChannelMessage(channel.ID, channel.LastMessageID)
	if err == nil {
		result = append(result, message)
	}
	//It's OK for there to be an error--some channels don't have a starter message anyway
	fmt.Printf("Fetching messages for IDF for %v (%v)\n", channel.Name, channel.ID)
	lastMessageID := channel.LastMessageID
	continueFetching := true
	for continueFetching {
		fmt.Println("Fetching a batch of messages before " + lastMessageID)
		//lastMessageID will be excluded
		messages, err := session.ChannelMessages(channel.ID, MESSAGES_TO_FETCH, lastMessageID, "", "")
		if err != nil {
			return nil, fmt.Errorf("couldn't fetch messages around %v: %w", lastMessageID, err)
		}
		if len(messages) == 0 {
			break
		}
		if len(messages) < MESSAGES_TO_FETCH {
			//This must have been the last batch to fetch
			continueFetching = false
		}
		result = append(result, messages...)
		//Messages are sorted with most recent first and least recent last.
		lastMessageID = messages[len(messages)-1].ID
	}
	return result, nil
}

func newIDFIndex(guildID string) *IDFIndex {
	data := &idfIndexJSON{
		DocumentCount:      0,
		DocumentWordCounts: make(map[string]int),
		FormatVersion:      IDF_JSON_FORMAT_VERSION,
	}
	return &IDFIndex{
		data:    data,
		guildID: guildID,
	}
}

func BuildIDFIndex(guildID string, session *discordgo.Session) (*IDFIndex, error) {

	result := newIDFIndex(guildID)

	guild, err := session.State.Guild(guildID)
	if err != nil {
		return nil, fmt.Errorf("couldn't fetch guild from state: %v", err)
	}
	fmt.Printf("Rebuilding IDF for Guild %v(%v)\n", guild.Name, guild.ID)
	for _, channel := range guild.Channels {
		if channel.Type != discordgo.ChannelTypeGuildText {
			continue
		}
		if channel.LastMessageID == "" {
			//No messages in channel at all!
			continue
		}
		messages, err := FetchAllMessagesForChannel(session, channel)
		if err != nil {
			return nil, fmt.Errorf("couldn't fetch messages for channel %v: %v", channel.ID, err)
		}
		for _, message := range messages {
			result.ProcessMessage(message)
		}
	}
	fmt.Printf("Done rebuilding IDF for Guild %v(%v)\n", guild.Name, guild.ID)

	//Save this so we don't have to do it again later
	if err := result.Persist(); err != nil {
		//This is not a problem to report that widely
		fmt.Printf("couldn't persist idf index for guildID %v: %v", guildID, err)
	}

	return result, nil
}

//Persist persists the cache to disk. Load it back up later with guildID.
func (i *IDFIndex) Persist() error {
	if i.guildID == "" {
		return fmt.Errorf("IDF index had no guildID")
	}
	folderPath := filepath.Join(CACHE_PATH, IDF_CACHE_PATH)
	path := filepath.Join(folderPath, i.guildID+".json")
	blob, err := json.MarshalIndent(i.data, "", "\t")
	if err != nil {
		return fmt.Errorf("couldnt format json: %w", err)
	}
	if _, err := os.Stat(folderPath); os.IsNotExist(err) {
		if err := os.MkdirAll(folderPath, 0700); err != nil {
			return fmt.Errorf("couldn't create cache folder: %w", err)
		}
	}
	return ioutil.WriteFile(path, blob, 0644)
}

func (i *IDFIndex) IDFForStemmedWord(stemmedWord string) float64 {
	//idf (inverse document frequency) of every word in the corpus. See
	//https://en.wikipedia.org/wiki/Tf%E2%80%93idf
	return math.Log10(float64(i.data.DocumentCount) / (float64(i.data.DocumentWordCounts[stemmedWord]) + 1))
}

func (i *IDFIndex) DocumentCount() int {
	return i.data.DocumentCount
}

//ProcessMessage will process a given message and update the index.
func (i *IDFIndex) ProcessMessage(message *discordgo.Message) {
	if message == nil {
		return
	}
	//Skip messages that are not from users
	if message.Type != discordgo.MessageTypeDefault && message.Type != discordgo.MessageTypeReply {
		return
	}
	words := extractWordsFromContent(message.Content)

	wordSet := make(map[string]bool)

	for _, word := range words {
		wordSet[word] = true
	}

	for word := range wordSet {
		i.data.DocumentWordCounts[word] += 1
	}

	i.data.DocumentCount++
}

var IMPORTANT_REACTIONS = map[string]float64{
	"🎯": 0.5,
	"🤯": 1.0,
	"💎": 1.0,
	"💯": 0.5,
}

func (i *IDFIndex) TFIDFForMessages(messages ...*discordgo.Message) *TFIDF {
	tfidf := make(map[string]float64)

	subCounts := make(map[string]float64)

	for _, message := range messages {
		multiplier := 1.0
		for _, reaction := range message.Reactions {
			multiplier += IMPORTANT_REACTIONS[reaction.Emoji.Name]
		}
		for _, word := range extractWordsFromContent(message.Content) {
			subCounts[word] += 1
		}
		for word, subCount := range subCounts {
			tfidf[word] += subCount * multiplier
		}
	}

	for word, value := range tfidf {
		tfidf[word] = value * i.IDFForStemmedWord(word)
	}

	return &TFIDF{
		values:   tfidf,
		messages: messages,
	}
}
