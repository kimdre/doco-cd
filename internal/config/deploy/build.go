package deploy

// BuildConfig holds build options for a deployment.
type BuildConfig struct {
	ForceImagePull bool              `yaml:"force_image_pull" json:"force_image_pull" default:"false"` // ForceImagePull always attempt to pull a newer version of the image
	Quiet          bool              `yaml:"quiet" json:"quiet" default:"false"`                       // Quiet suppresses the build output
	Args           map[string]string `yaml:"args" json:"args"`                                         // BuildArgs is a map of build-time arguments to pass to the build process
	NoCache        bool              `yaml:"no_cache" json:"no_cache" default:"false"`                 // NoCache disables the use of the cache when building images
}
