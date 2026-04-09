package core

import (
	"fmt"
	"strconv"

	"github.com/bogem/id3v2/v2"
)

// MP3Tagger handles metadata tagging for MP3 files using ID3v2
type MP3Tagger struct{}

// NewMP3Tagger creates a new MP3 tagger
func NewMP3Tagger() *MP3Tagger {
	return &MP3Tagger{}
}

// TagFile applies metadata to an MP3 file using ID3v2 tags
func (t *MP3Tagger) TagFile(path string, metadata TrackMetadata) error {
	tag, err := id3v2.Open(path, id3v2.Options{Parse: true})
	if err != nil {
		return fmt.Errorf("failed to open MP3 file: %w", err)
	}
	defer tag.Close()

	tag.SetDefaultEncoding(id3v2.EncodingUTF8)

	if metadata.Title != "" {
		tag.SetTitle(metadata.Title)
	}
	if metadata.Artist != "" {
		tag.SetArtist(metadata.Artist)
	}
	if metadata.Album != "" {
		tag.SetAlbum(metadata.Album)
	}
	if metadata.AlbumArtist != "" {
		// TPE2 = Album artist / Band
		tag.AddTextFrame(tag.CommonID("Band/Orchestra/Accompaniment"), id3v2.EncodingUTF8, metadata.AlbumArtist)
	}
	if metadata.TrackNumber > 0 {
		trackText := strconv.Itoa(metadata.TrackNumber)
		if metadata.TotalTracks > 0 {
			trackText += "/" + strconv.Itoa(metadata.TotalTracks)
		}
		tag.AddTextFrame(tag.CommonID("Track number/Position in set"), id3v2.EncodingUTF8, trackText)
	}
	if metadata.DiscNumber > 0 {
		discText := strconv.Itoa(metadata.DiscNumber)
		if metadata.TotalDiscs > 0 {
			discText += "/" + strconv.Itoa(metadata.TotalDiscs)
		}
		tag.AddTextFrame(tag.CommonID("Part of a set"), id3v2.EncodingUTF8, discText)
	}
	if metadata.Year != "" {
		tag.SetYear(metadata.Year)
	}
	if metadata.Genre != "" {
		tag.SetGenre(metadata.Genre)
	}
	if metadata.ISRC != "" {
		tag.AddTextFrame("TSRC", id3v2.EncodingUTF8, metadata.ISRC)
	}
	if metadata.Label != "" {
		tag.AddTextFrame("TPUB", id3v2.EncodingUTF8, metadata.Label)
	}
	if metadata.Copyright != "" {
		tag.AddTextFrame("TCOP", id3v2.EncodingUTF8, metadata.Copyright)
	}
	if metadata.Composer != "" {
		tag.AddTextFrame("TCOM", id3v2.EncodingUTF8, metadata.Composer)
	}

	// Embed cover art as APIC frame (front cover)
	if len(metadata.CoverArt) > 0 {
		pic := id3v2.PictureFrame{
			Encoding:    id3v2.EncodingUTF8,
			MimeType:    "image/jpeg",
			PictureType: id3v2.PTFrontCover,
			Description: "Front cover",
			Picture:     metadata.CoverArt,
		}
		// Detect PNG
		if len(metadata.CoverArt) > 4 &&
			metadata.CoverArt[0] == 0x89 && metadata.CoverArt[1] == 'P' &&
			metadata.CoverArt[2] == 'N' && metadata.CoverArt[3] == 'G' {
			pic.MimeType = "image/png"
		}
		tag.AddAttachedPicture(pic)
	}

	// Embed lyrics as USLT frame
	if metadata.Lyrics != "" {
		uslt := id3v2.UnsynchronisedLyricsFrame{
			Encoding:          id3v2.EncodingUTF8,
			Language:          "eng",
			ContentDescriptor: "",
			Lyrics:            metadata.Lyrics,
		}
		tag.AddUnsynchronisedLyricsFrame(uslt)
	}
	if metadata.SyncedLyrics != "" {
		uslt := id3v2.UnsynchronisedLyricsFrame{
			Encoding:          id3v2.EncodingUTF8,
			Language:          "eng",
			ContentDescriptor: "synced",
			Lyrics:            metadata.SyncedLyrics,
		}
		tag.AddUnsynchronisedLyricsFrame(uslt)
	}

	if err := tag.Save(); err != nil {
		return fmt.Errorf("failed to save ID3v2 tags: %w", err)
	}

	return nil
}

// TagMP3File creates an MP3Tagger and tags the file with the given metadata
func TagMP3File(path string, metadata TrackMetadata) error {
	tagger := NewMP3Tagger()
	return tagger.TagFile(path, metadata)
}
