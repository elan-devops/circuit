package codeamp

import (
	"fmt"

	"github.com/jinzhu/gorm"

	"github.com/codeamp/circuit/plugins/codeamp/model"
	log "github.com/codeamp/logger"
	"github.com/codeamp/transistor"
	"github.com/spf13/viper"
)

// ComplainIfNotInStaging will send out a notification event
// if the input release has been deployed to constants.ProductionEnvironment
// without a corresponding release in constants.StagingEnvironment
// returns true/false on whether the function "complained" or not
func (x *CodeAmp) ComplainIfNotInStaging(r *model.Release, p *model.Project) (bool, error) {
	complained := false
	enabled := viper.GetBool("plugins.codeamp.complainer.enabled")
	parentEnvironment := viper.GetString("plugins.codeamp.complainer.environments.parent")
	childEnvironment := viper.GetString("plugins.codeamp.complainer.environments.child")

	if !enabled {
		return false, nil
	}

	// get staging and production environments
	stagingEnv := model.Environment{}
	if err := x.DB.Where("key = ?", parentEnvironment).Find(&stagingEnv).Error; err != nil {
		return complained, err
	}

	prodEnv := model.Environment{}
	if err := x.DB.Where("key = ?", childEnvironment).Find(&prodEnv).Error; err != nil {
		return complained, err
	}

	// check if input release's environment matches prodEnv. if it doesn't, then there's nothing to complain about
	// as we only care about releases going to prod environment that they have a corresponding
	// successful, staging environment release
	inputReleaseEnv := model.Environment{}
	if err := x.DB.Where("id = ?", r.EnvironmentID).Find(&inputReleaseEnv).Error; err != nil {
		return complained, err
	}

	if inputReleaseEnv.Key != prodEnv.Key {
		return complained, nil
	}

	f := model.Feature{}
	if err := x.DB.Where("id = ?", r.HeadFeatureID).Find(&f).Error; err != nil {
		return complained, err
	}

	// get a list of all features that were created after the input release's feature
	features := []model.Feature{}
	if err := x.DB.Where("created_at >= ?", f.Model.CreatedAt).Find(&features).Error; err != nil {
		return complained, err
	}

	// prepend input release's head feature into features, so it's inclusive of the input release's
	// head feature when finding a corresponding staging release
	features = append([]model.Feature{f}, features...)

	releaseFoundInStaging := false
	for _, feature := range features {
		// check if successful release exists in staging environment
		stagingRelease := model.Release{}
		if err := x.DB.Where("state = ? and head_feature_id = ? and environment_id = ?", transistor.GetState("complete"), feature.Model.ID, stagingEnv.Model.ID).Find(&stagingRelease).Error; err != nil {
			// if it's any error besides IsRecordNotFound, terminate the function execution here
			if !gorm.IsRecordNotFoundError(err) {
				return complained, err
			}
		} else {
			// a successful release in staging is found
			releaseFoundInStaging = true
			break
		}
	}

	releaseUser := model.User{}
	if err := x.DB.Where("id = ?", r.UserID).Find(&releaseUser).Error; err != nil {
		return complained, err
	}

	if !releaseFoundInStaging {
		// send notification
		complaint := fmt.Sprintf("This feature was deployed directly to %s without a prior deployment in %s.", prodEnv.Name, stagingEnv.Name)
		log.InfoWithFields(complaint, log.Fields{
			"ProjectID": p.Model.ID,
			"ReleaseID": r.Model.ID,
		})
		x.SendNotifications(complaint, r, p)
		complained = true
	}

	return complained, nil
}
