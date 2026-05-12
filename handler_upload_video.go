package main

import (
	"bytes"
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

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
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
	ratio, err := getVideoAspectRatio(f.Name())
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
		Body:        f,
		ContentType: &mediaType,
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "error uploading videofile", err)
	}

	newVideoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, newFilename)
	video.VideoURL = &newVideoURL
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "error uploading videofile", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
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
