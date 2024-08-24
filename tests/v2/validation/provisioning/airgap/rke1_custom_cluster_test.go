//go:build validation

package airgap

import (
	"testing"

	"github.com/rancher/rancher/tests/v2/validation/pipeline/rancherha/corralha"
	"github.com/rancher/rancher/tests/v2/validation/provisioning/permutations"
	"github.com/rancher/rancher/tests/v2/validation/provisioning/registries"
	"github.com/rancher/shepherd/clients/corral"
	"github.com/rancher/shepherd/clients/rancher"
	"github.com/rancher/shepherd/extensions/clusters"
	"github.com/rancher/shepherd/extensions/clusters/kubernetesversions"
	provisioning "github.com/rancher/shepherd/extensions/provisioning"
	"github.com/rancher/shepherd/extensions/provisioninginput"
	"github.com/rancher/shepherd/extensions/reports"
	"github.com/rancher/shepherd/pkg/config"
	"github.com/rancher/shepherd/pkg/session"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

type AirGapRKE1CustomClusterTestSuite struct {
	suite.Suite
	client         *rancher.Client
	session        *session.Session
	corralPackage  *corral.Packages
	clustersConfig *provisioninginput.Config
	registryFQDN   string
}

func (a *AirGapRKE1CustomClusterTestSuite) TearDownSuite() {
	a.session.Cleanup()
}

func (a *AirGapRKE1CustomClusterTestSuite) SetupSuite() {
	testSession := session.NewSession()
	a.session = testSession

	a.clustersConfig = new(provisioninginput.Config)
	config.LoadConfig(provisioninginput.ConfigurationFileKey, a.clustersConfig)

	corralRancherHA := new(corralha.CorralRancherHA)
	config.LoadConfig(corralha.CorralRancherHAConfigConfigurationFileKey, corralRancherHA)

	registriesConfig := new(registries.Registries)
	config.LoadConfig(registries.RegistriesConfigKey, registriesConfig)

	client, err := rancher.NewClient("", testSession)
	require.NoError(a.T(), err)

	a.client = client
	listOfCorrals, err := corral.ListCorral()
	require.NoError(a.T(), err)

	corralConfig := corral.Configurations()

	err = corral.SetupCorralConfig(corralConfig.CorralConfigVars, corralConfig.CorralConfigUser, corralConfig.CorralSSHPath)
	require.NoError(a.T(), err)

	a.corralPackage = corral.PackagesConfig()

	_, corralExist := listOfCorrals[corralRancherHA.Name]
	if corralExist {
		bastionIP, err := corral.GetCorralEnvVar(corralRancherHA.Name, corralRegistryIP)
		require.NoError(a.T(), err)

		err = corral.UpdateCorralConfig(corralBastionIP, bastionIP)
		require.NoError(a.T(), err)

		registryFQDN, err := corral.GetCorralEnvVar(corralRancherHA.Name, corralRegistryFQDN)
		require.NoError(a.T(), err)
		logrus.Infof("registry fqdn is %s", registryFQDN)

		err = corral.SetCorralSSHKeys(corralRancherHA.Name)
		require.NoError(a.T(), err)
		a.registryFQDN = registryFQDN
	} else {
		a.registryFQDN = registriesConfig.ExistingNoAuthRegistryURL
	}

}

func (a *AirGapRKE1CustomClusterTestSuite) TestProvisioningAirGapRKE1CustomCluster() {
	a.clustersConfig.NodePools = []provisioninginput.NodePools{provisioninginput.AllRolesNodePool}

	tests := []struct {
		name   string
		client *rancher.Client
	}{
		{provisioninginput.AdminClientName.String() + "-" + permutations.RKE1AirgapCluster + "-", a.client},
	}
	for _, tt := range tests {
		permutations.RunTestPermutations(&a.Suite, tt.name, tt.client, a.clustersConfig, permutations.RKE1AirgapCluster, nil, a.corralPackage)
	}

}

func (a *AirGapRKE1CustomClusterTestSuite) TestProvisioningUpgradeAirGapRKE1CustomCluster() {
	a.clustersConfig.NodePools = []provisioninginput.NodePools{provisioninginput.AllRolesNodePool}

	rke1Versions, err := kubernetesversions.ListRKE1AllVersions(a.client)
	require.NoError(a.T(), err)

	numOfRKE1Versions := len(rke1Versions)
	// for this we will only have one custom cluster entry and one cni entry
	require.Equal(a.T(), len(a.clustersConfig.CNIs), 1)

	a.clustersConfig.K3SKubernetesVersions[0] = rke1Versions[numOfRKE1Versions-2]

	testConfig := clusters.ConvertConfigToClusterConfig(a.clustersConfig)
	testConfig.KubernetesVersion = a.clustersConfig.RKE1KubernetesVersions[0]
	testConfig.CNI = a.clustersConfig.CNIs[0]
	clusterObject, err := provisioning.CreateProvisioningRKE1AirgapCustomCluster(a.client, testConfig, a.corralPackage)
	reports.TimeoutRKEReport(clusterObject, err)
	require.NoError(a.T(), err)

	provisioning.VerifyRKE1Cluster(a.T(), a.client, testConfig, clusterObject)

	upgradedCluster, err := provisioning.UpgradeClusterK8sVersion(a.client, &clusterObject.Name, &rke1Versions[numOfRKE1Versions-1])
	reports.TimeoutRKEReport(clusterObject, err)
	require.NoError(a.T(), err)

	provisioning.VerifyUpgrade(a.T(), upgradedCluster, rke1Versions[numOfRKE1Versions-1])
}

// In order for 'go test' to run this suite, we need to create
// a normal test function and pass our suite to suite.Run
func TestAirGapCustomClusterRKE1ProvisioningTestSuite(t *testing.T) {
	suite.Run(t, new(AirGapRKE1CustomClusterTestSuite))
}
