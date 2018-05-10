scionApp
    .controller('verificationCtrl', ['$scope', '$routeParams', '$location','verificationService', 
        function ($scope, $routeParams, $location, verificationService) {

            verificationService.verifyEmail($routeParams.uuid).then(
                function (response){ 
                    $scope.firstName = response.data.firstname;
                    $scope.lastName = response.data.lastname;
                },
                function (response){
                    $location.path('/login');
                });
        }
    ]);
