provider "google" {
  project = var.gcp_project_id
  region  = var.gcp_region
  zone    = var.gcp_zone
}

resource "random_pet" "suffix" {}

locals {
  name = "flux-test-${random_pet.suffix.id}"
}

module "gke" {
  source = "git::https://gitlab.com/darkowlzz/flux-test-infra.git//modules/gcp/gke"

  name = local.name
}

module "gcr" {
  source = "git::https://gitlab.com/darkowlzz/flux-test-infra.git//modules/gcp/gcr"

  name = local.name
}
