package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {

	http.MaxBytesReader(w, r.Body, 1<<30)
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "video not found", err)
		return
	}

	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Unauthorized user", nil)
		return
	}

	videoFile, videoHeader, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "unable to parse form files", err)
		return
	}

	defer videoFile.Close()

	mediaType := videoHeader.Header.Get("Content-Type")
	mediaType, _, err = mime.ParseMediaType(mediaType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "could not parse mediaType", err)
		return
	}

	mediaType, _, err = mime.ParseMediaType(mediaType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "could not parse mediaType", err)
		return
	}

	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "wrong media type parse", nil)
		return
	}

	f, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "error handling video file", err)
		return
	}

	defer os.Remove(f.Name())
	defer f.Close()

	_, err = io.Copy(f, videoFile)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "error handling video file", err)
	}
	_, err = f.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "error handling video file", err)
		return
	}

	fProcessed, err := processVideoForFastStart(f.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "error processing video file", err)
		return
	}
	newF, err := os.Open(fProcessed)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "error processing video file", err)
		return
	}
	defer os.Remove(newF.Name())
	defer newF.Close()

	ratio, err := getVideoAspectRatio(newF.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "error reading file metadata", err)
		return
	}
	var orientation string
	switch ratio {
	case "9:16":
		orientation = "portrait"
	case "16:9":
		orientation = "landscape"
	default:
		orientation = "other"
	}

	fileID := make([]byte, 32)
	rand.Read(fileID)
	fileIDString := base64.RawURLEncoding.EncodeToString(fileID)
	headerSlice := strings.Split(mediaType, "/")
	fileExtension := headerSlice[len(headerSlice)-1]
	newFilename := fmt.Sprintf("%s/%s.%s", orientation, fileIDString, fileExtension)

	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &newFilename,
		Body:        newF,
		ContentType: &mediaType,
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "error uploading videofile", err)
		return
	}

	newVideoURL := fmt.Sprintf("%s,%s", cfg.s3Bucket, newFilename)
	video.VideoURL = &newVideoURL
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "error uploading videofile", err)
		return
	}

	presignedVideo, err := cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "error getting presignedURL", err)
		return
	}
	respondWithJSON(w, http.StatusOK, presignedVideo)
}
func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	presignClient := s3.NewPresignClient(s3Client)

	PresignedURLRequest, err := presignClient.PresignGetObject(
		context.Background(),
		&s3.GetObjectInput{
			Bucket: &bucket,
			Key:    &key,
		},
		s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", err
	}

	return PresignedURLRequest.URL, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return video, nil
	}

	bucketKey := strings.Split(*video.VideoURL, ",")
	// videoURL is stored as <bucket>,<key>
	presignedURL, err := generatePresignedURL(cfg.s3Client, bucketKey[0], bucketKey[1], time.Hour)
	if err != nil {
		return database.Video{}, err
	}
	video.VideoURL = &presignedURL
	return video, nil
}

type VideoStream struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type StreamList struct {
	Streams []VideoStream `json:"streams"`
}

func getVideoAspectRatio(filepath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filepath)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	var result StreamList
	err = json.Unmarshal(buf.Bytes(), &result)
	if err != nil {
		return "", err
	}
	if len(result.Streams) == 0 {
		return "", fmt.Errorf("no Stream output for command")
	}
	videoHeight := float64(result.Streams[0].Height)
	videoWidth := float64(result.Streams[0].Width)
	aspectRatio := videoWidth / videoHeight
	tolerance := 1e-3
	switch {
	case withinDelta(aspectRatio, 9.0/16.0, tolerance):
		return "9:16", nil
	case withinDelta(aspectRatio, 16.0/9.0, tolerance):
		return "16:9", nil
	default:
		return "other", nil
	}
}

func withinDelta(a, b, tolerance float64) bool {
	return math.Abs(a-b) <= tolerance
}

func processVideoForFastStart(filepath string) (string, error) {
	// moov atom for mp4 files is generally at the "end" of the file.
	// this function creates a new video that moves it to the start of the file.
	outputFilepath := filepath + ".processing"
	cmd := exec.Command("ffmpeg", "-i", filepath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outputFilepath)
	err := cmd.Run()
	if err != nil {
		return "", err
	}

	return outputFilepath, nil
}
