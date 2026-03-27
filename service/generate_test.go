package service

//go:generate moq -rm -out mock_hypervisor_test.go -pkg service ../hypervisor Hypervisor Direct
//go:generate moq -rm -out mock_images_test.go -pkg service ../images Images
//go:generate moq -rm -out mock_network_test.go -pkg service ../network Network
//go:generate moq -rm -out mock_snapshot_test.go -pkg service ../snapshot Snapshot
