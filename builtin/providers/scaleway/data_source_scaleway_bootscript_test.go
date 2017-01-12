package scaleway

import (
	"fmt"
	"os"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform/helper/resource"
	"github.com/hashicorp/terraform/terraform"
	"github.com/scaleway/scaleway-cli/pkg/api"
	"github.com/scaleway/scaleway-cli/pkg/scwversion"
)

func TestAccScalewayDataSourceBootscript_Basic(t *testing.T) {
	testAccPreCheck(t)
	client, err := api.NewScalewayAPI(
		os.Getenv("SCALEWAY_ORGANIZATION"),
		os.Getenv("SCALEWAY_ACCESS_KEY"),
		scwversion.UserAgent(),
		"par1",
	)
	if err != nil {
		t.Fatal(err)
	}
	testAccProvider.SetMeta(&client)
	bootscripts, err := client.GetBootscripts()
	if err != nil {
		t.Fatal(err)
	}
	bootscript := (*bootscripts)[0]

	resource.Test(t, resource.TestCase{
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			resource.TestStep{
				Config: fmt.Sprintf(testAccCheckScalewayBootscriptConfig, bootscript.Title, bootscript.Arch),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckBootscriptID("data.scaleway_bootscript.debug"),
					resource.TestCheckResourceAttr("data.scaleway_bootscript.debug", "architecture", bootscript.Arch),
					resource.TestCheckResourceAttr("data.scaleway_bootscript.debug", "public", "true"),
					resource.TestMatchResourceAttr("data.scaleway_bootscript.debug", "kernel", regexp.MustCompile(bootscript.Kernel)),
				),
			},
		},
	})
}

func TestAccScalewayDataSourceBootscript_Filtered(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:  func() { testAccPreCheck(t) },
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			resource.TestStep{
				Config: testAccCheckScalewayBootscriptFilterConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckBootscriptID("data.scaleway_bootscript.debug"),
					resource.TestCheckResourceAttr("data.scaleway_bootscript.debug", "architecture", "arm"),
					resource.TestCheckResourceAttr("data.scaleway_bootscript.debug", "public", "true"),
				),
			},
		},
	})
}

func testAccCheckBootscriptID(n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("Can't find bootscript data source: %s", n)
		}

		if rs.Primary.ID == "" {
			return fmt.Errorf("bootscript data source ID not set")
		}

		scaleway := testAccProvider.Meta().(*Client).scaleway
		_, err := scaleway.GetBootscript(rs.Primary.ID)
		if err != nil {
			return err
		}

		return nil
	}
}

const testAccCheckScalewayBootscriptConfig = `
data "scaleway_bootscript" "debug" {
  name = "%s"
  architecture = "%s"
}
`

const testAccCheckScalewayBootscriptFilterConfig = `
data "scaleway_bootscript" "debug" {
  architecture = "arm"
  name_filter = "Rescue"
}
`
